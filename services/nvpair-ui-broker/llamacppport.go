// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"nvpair-shared/errors"
)

const (
	managedLlamaCppFacadePort      = 8080
	managedLlamaCppBackendStart    = 8081
	llamacppPortOwnershipBlockedID = "llamacpp-proxy:port-ownership-blocked"
)

// planManagedLlamaCppPorts keeps the ownership policy deterministic. llama.cpp
// runs as a process-mode engine that engine-manager owns outright: a session
// it started can be bounced onto a backend port via engine:set-port. An
// adopted external process (or an unknown owner) is still refused by
// engine:set-port, in which case preparation falls back instead of moving it.
func planManagedLlamaCppPorts(enabled bool, st ollamaPortStatus, available func(int) bool) managedPortPlan {
	if !enabled {
		return managedPortPlan{}
	}
	if st.Port > 0 && st.Port != managedLlamaCppFacadePort {
		if !available(managedLlamaCppFacadePort) {
			return managedPortPlan{Blocked: "the compatibility port is already in use"}
		}
		if !st.Running && !available(st.Port) {
			backend := nextAvailablePort(managedLlamaCppBackendStart, available)
			if backend == 0 {
				return managedPortPlan{Blocked: "no free backend port is available"}
			}
			return managedPortPlan{Enabled: true, BackendPort: backend}
		}
		return managedPortPlan{Enabled: true}
	}
	backend := nextAvailablePort(managedLlamaCppBackendStart, available)
	if backend == 0 {
		return managedPortPlan{Blocked: "no free backend port is available"}
	}
	return managedPortPlan{Enabled: true, BackendPort: backend}
}

func (b *Broker) markLlamaCppPortReady() {
	if b.llamacppPortReady != nil {
		b.llamacppPortReadyOnce.Do(func() { close(b.llamacppPortReady) })
	}
}

func (b *Broker) llamacppPortOwnershipPending() bool {
	if b.llamacppPortReady == nil {
		return false
	}
	select {
	case <-b.llamacppPortReady:
		return false
	default:
		return true
	}
}

func needsLlamaCppPortGate(method string, params json.RawMessage) bool {
	if method == "engine:get-installed" {
		return true
	}
	if method != "engine:status" && method != "engine:start" && method != "engine:restart" {
		return false
	}
	var request struct {
		Engine string `json:"engine"`
	}
	return json.Unmarshal(params, &request) == nil && request.Engine == "llamacpp"
}

func llamacppSetPortRequest(method string, params json.RawMessage) (int, bool) {
	if method != "engine:set-port" {
		return 0, false
	}
	var request struct {
		Engine string `json:"engine"`
		Port   int    `json:"port"`
	}
	if json.Unmarshal(params, &request) != nil || request.Engine != "llamacpp" || request.Port <= 0 {
		return 0, false
	}
	return request.Port, true
}

func (b *Broker) reportLlamaCppPortOwnershipBlocked(reason string) {
	b.forwardErrorsReport(errors.ServiceError{
		ID:        llamacppPortOwnershipBlockedID,
		Message:   fmt.Sprintf("NVPAIR could not safely reserve llama.cpp port %d: %s. No unknown process was stopped.", managedLlamaCppFacadePort, reason),
		Timestamp: nowMillis(),
		NodeID:    b.nodeID,
		Severity:  "warning",
		Action:    "none",
	})
}

func (b *Broker) setLlamaCppProxyFallback(excludedPorts ...int) int {
	if aliasPort := b.currentOllamaHostAlias().Port; aliasPort > 0 {
		excludedPorts = append(excludedPorts, aliasPort)
	}
	if backend := int(b.llamacppBackendPort.Load()); backend > 0 {
		excludedPorts = append(excludedPorts, backend)
	} else {
		// With no authoritative backend yet, neither llama.cpp's compatibility
		// port nor engine-manager's bundled backend default is safe evidence of a
		// free proxy port.
		excludedPorts = append(excludedPorts, managedLlamaCppFacadePort, managedLlamaCppBackendStart)
	}
	fallback := nextAvailablePortExcluding(managedLlamaCppBackendStart, excludedPorts, tcpPortAvailable)
	b.llamacppProxyStartupPort.Store(int32(fallback))
	return fallback
}

func (b *Broker) rebindLlamaCppProxy(p *proxyProcess, port int) bool {
	if p == nil || port == 0 {
		return false
	}
	body, _ := json.Marshal(map[string]int{"port": port})
	result, rpcErr, err := p.Call(context.Background(), "set-port", body)
	if err != nil || rpcErr != nil {
		slog.Warn("failed to rebind llama.cpp proxy", "port", port, "err", err, "rpcErr", rpcErr)
		return false
	}
	var ready proxyReadyParams
	return json.Unmarshal(result, &ready) == nil && ready.Port == port
}

// blockManagedLlamaCppFacade records an explicit fallback and optionally
// rebinds a live proxy. It does not open the ownership gate: only a confirmed
// bound proxy generation or an exhausted supervisor may do that.
func (b *Broker) blockManagedLlamaCppFacade(reason string, p *proxyProcess, excludedPorts ...int) (int, bool) {
	b.managedLlamaCppFacade.Store(false)
	fallback := b.setLlamaCppProxyFallback(excludedPorts...)
	b.reportLlamaCppPortOwnershipBlocked(reason)
	return fallback, b.rebindLlamaCppProxy(p, fallback)
}

func (b *Broker) cacheLlamaCppPortStatus() (ollamaPortStatus, bool) {
	em := b.getEngineMgr()
	if em == nil {
		return ollamaPortStatus{}, false
	}
	params, _ := json.Marshal(map[string]string{"engine": "llamacpp"})
	result, rpcErr, err := em.Call(context.Background(), "engine:status", params)
	if err != nil || rpcErr != nil {
		return ollamaPortStatus{}, false
	}
	var st ollamaPortStatus
	if json.Unmarshal(result, &st) != nil {
		return ollamaPortStatus{}, false
	}
	if st.Port <= 0 {
		return st, false
	}
	b.llamacppBackendPort.Store(int32(st.Port))
	return st, true
}

func (b *Broker) configureUnmanagedLlamaCppFacade() {
	b.managedLlamaCppFacade.Store(false)
	b.llamacppProxyStartupPort.Store(0)
	b.forwardErrorsClear(llamacppPortOwnershipBlockedID)
}

// prepareManagedLlamaCppFacade runs after engine-manager starts and before the
// llama.cpp proxy is spawned. Unlike Ollama, the backend move happens first:
// engine-manager bounces a session it started onto the backend port via
// engine:set-port. Only after :8080 is verified free does the
// broker force the proxy onto the compatibility port.
func (b *Broker) prepareManagedLlamaCppFacade() {
	b.prepareManagedLlamaCppFacadeWithPortCheck(tcpPortAvailable)
}

func (b *Broker) prepareManagedLlamaCppFacadeWithPortCheck(portAvailable func(int) bool) {
	// Ollama's facade is prepared first, so an inherited OLLAMA_HOST alias is
	// already reserved here and must stay out of llama.cpp's backend search.
	portAvailable = b.availableOffOllamaHostAlias(portAvailable)
	settings := b.getSettings()
	if settings == nil {
		b.cacheLlamaCppPortStatus()
		_, _ = b.blockManagedLlamaCppFacade("managed-port policy is unavailable", nil)
		return
	}
	result, rpcErr, err := settings.Call(context.Background(), "settings/get-force-ports", nil)
	if err != nil || rpcErr != nil {
		slog.Warn("failed to read managed llama.cpp port setting", "err", err, "rpcErr", rpcErr)
		b.cacheLlamaCppPortStatus()
		_, _ = b.blockManagedLlamaCppFacade("managed-port policy could not be verified", nil)
		return
	}
	var policy struct {
		Value bool `json:"value"`
	}
	if json.Unmarshal(result, &policy) != nil {
		b.cacheLlamaCppPortStatus()
		_, _ = b.blockManagedLlamaCppFacade("managed-port policy could not be decoded", nil)
		return
	}

	em := b.getEngineMgr()
	if em == nil {
		if !policy.Value {
			b.configureUnmanagedLlamaCppFacade()
			return
		}
		_, _ = b.blockManagedLlamaCppFacade("engine manager is unavailable", nil)
		return
	}

	params, _ := json.Marshal(map[string]any{"engine": "llamacpp", "port": managedLlamaCppFacadePort})
	result, rpcErr, err = em.Call(context.Background(), "engine:status", params)
	if err != nil || rpcErr != nil {
		if !policy.Value {
			b.configureUnmanagedLlamaCppFacade()
			return
		}
		_, _ = b.blockManagedLlamaCppFacade("llama.cpp status could not be verified", nil)
		return
	}
	var st ollamaPortStatus
	if json.Unmarshal(result, &st) != nil {
		if !policy.Value {
			b.configureUnmanagedLlamaCppFacade()
			return
		}
		_, _ = b.blockManagedLlamaCppFacade("llama.cpp status could not be decoded", nil)
		return
	}
	if st.Port > 0 {
		b.llamacppBackendPort.Store(int32(st.Port))
	}
	if !policy.Value {
		b.configureUnmanagedLlamaCppFacade()
		return
	}

	plan := planManagedLlamaCppPorts(true, st, portAvailable)
	if plan.Blocked != "" {
		_, _ = b.blockManagedLlamaCppFacade(plan.Blocked, nil, st.Port)
		return
	}
	if plan.BackendPort != 0 {
		params, _ = json.Marshal(map[string]any{"engine": "llamacpp", "port": plan.BackendPort})
		result, rpcErr, err = em.CallNoTimeout(context.Background(), "engine:set-port", params)
		if err != nil || rpcErr != nil {
			b.cacheLlamaCppPortStatus()
			_, _ = b.blockManagedLlamaCppFacade("the llama.cpp backend could not be moved", nil, st.Port, plan.BackendPort)
			return
		}
		b.llamacppBackendPort.Store(int32(plan.BackendPort))
		var moved ollamaPortStatus
		if json.Unmarshal(result, &moved) == nil && moved.Port > 0 {
			b.llamacppBackendPort.Store(int32(moved.Port))
		}
	}
	if !portAvailable(managedLlamaCppFacadePort) {
		_, _ = b.blockManagedLlamaCppFacade("the compatibility port is already in use", nil, st.Port)
		return
	}

	b.managedLlamaCppFacade.Store(plan.Enabled)
	b.llamacppProxyStartupPort.Store(managedLlamaCppFacadePort)
	b.forwardErrorsClear(llamacppPortOwnershipBlockedID)
}

func (b *Broker) llamacppProxyGenerationIsCurrent(generation uint64, p *proxyProcess) bool {
	return b.llamacppProxyGeneration.Load() == generation &&
		b.llamacppProxyPublishedGeneration.Load() == generation &&
		b.getLlamaCppProxy() == p
}

// invalidateLlamaCppProxyForRestartLocked invalidates the current logical
// generation and broker-visible handle. Caller holds llamacppReadyMu.
func (b *Broker) invalidateLlamaCppProxyForRestartLocked(generation uint64) bool {
	if b.llamacppProxyGeneration.Load() != generation {
		return false
	}
	b.llamacppProxyGeneration.Add(1)
	b.llamacppProxyPublishedGeneration.Store(0)
	b.setLlamaCppProxy(nil)
	return true
}

func (b *Broker) restartLlamaCppProxyOrFinish(generation uint64) {
	// Caller holds llamacppReadyMu. Invalidate before the restart request
	// becomes observable so a ready notification already queued by the failed
	// process cannot complete the gate.
	if !b.invalidateLlamaCppProxyForRestartLocked(generation) {
		return
	}
	if b.llamacppProxySup != nil {
		b.llamacppProxySup.Restart()
		return
	}
	b.finishLlamaCppProxyTerminal()
}

func (b *Broker) reconcileLlamaCppProxyAfterEngineManagerReady() {
	if !b.llamacppPortOwnershipPending() {
		return
	}
	p := b.getLlamaCppProxy()
	if p == nil {
		return
	}
	ready, port := p.Status()
	generation := b.llamacppProxyPublishedGeneration.Load()
	if !ready || port <= 0 || generation != b.llamacppProxyGeneration.Load() {
		return
	}
	go b.reconcileLlamaCppProxyPortOnReadyForGeneration(generation, port)
}

// reconcileLlamaCppProxyPortOnReady runs off the proxy reader goroutine because
// both set-port and node/set-local-backend round-trip through that reader.
func (b *Broker) reconcileLlamaCppProxyPortOnReady(boundPort int) {
	b.reconcileLlamaCppProxyPortOnReadyForGeneration(b.llamacppProxyGeneration.Load(), boundPort)
}

func (b *Broker) reconcileLlamaCppProxyPortOnReadyForGeneration(generation uint64, boundPort int) {
	if b.llamacppProxyGeneration.Load() != generation {
		return
	}
	// Serialize the ownership transition, including fallback rebind. A set-port
	// emits another ready before its response, so without this guard that second
	// callback could release the gate while the first callback was still moving
	// the proxy. No caller holds another broker lock when entering this method.
	b.llamacppReadyMu.Lock()
	defer b.llamacppReadyMu.Unlock()

	p := b.getLlamaCppProxy()
	if p == nil || !b.llamacppProxyGenerationIsCurrent(generation, p) {
		return
	}
	if b.managedLlamaCppFacade.Load() && boundPort != managedLlamaCppFacadePort {
		if b.rebindLlamaCppProxy(p, managedLlamaCppFacadePort) {
			boundPort = managedLlamaCppFacadePort
		} else {
			fallback, rebound := b.blockManagedLlamaCppFacade("the proxy could not bind the compatibility port", p, boundPort)
			if !rebound {
				b.restartLlamaCppProxyOrFinish(generation)
				return
			}
			boundPort = fallback
		}
	}

	backend := int(b.llamacppBackendPort.Load())
	if backend == 0 {
		// Fail closed on the bundled backend default before asking
		// engine-manager for status. Its identity probe is also rejected by the
		// facade, but moving first avoids even transiently probing this proxy.
		if boundPort == managedLlamaCppBackendStart {
			fallback := b.setLlamaCppProxyFallback(boundPort)
			if !b.rebindLlamaCppProxy(p, fallback) {
				b.restartLlamaCppProxyOrFinish(generation)
				return
			}
			boundPort = fallback
		}
		backend = defaultLlamaCppPort
	}
	avoidBackendCollision := func() bool {
		if backend != boundPort {
			return true
		}
		var fallback int
		var rebound bool
		if b.managedLlamaCppFacade.Load() {
			fallback, rebound = b.blockManagedLlamaCppFacade("the proxy bound the configured llama.cpp backend port", p, boundPort)
		} else {
			fallback = b.setLlamaCppProxyFallback(boundPort)
			rebound = b.rebindLlamaCppProxy(p, fallback)
		}
		if !rebound {
			b.restartLlamaCppProxyOrFinish(generation)
			return false
		}
		boundPort = fallback
		return true
	}
	// Never probe engine-manager while the proxy is sitting on the configured
	// backend: that probe could adopt the proxy as llama.cpp. Move the proxy
	// first, then refresh authoritative engine state.
	if !avoidBackendCollision() {
		return
	}

	st, current := b.cacheLlamaCppPortStatus()
	if !b.llamacppProxyGenerationIsCurrent(generation, p) {
		return
	}
	cached := int(b.llamacppBackendPort.Load())
	if cached <= 0 {
		// A bound proxy plus an unknown configured backend is not a terminal
		// ownership result. Keep restoration gated until engine-manager (or a
		// replacement manager) supplies the authoritative port.
		slog.Warn("llama.cpp backend port remains unknown; keeping ownership gate closed")
		return
	}
	backend = cached
	if !avoidBackendCollision() {
		return
	}
	healthy := current && st.Running && st.Port == backend
	if !b.llamacppProxyGenerationIsCurrent(generation, p) {
		return
	}
	b.setProxyLocalBackend(p, "llamacpp", backend, healthy)
	if !b.llamacppProxyGenerationIsCurrent(generation, p) {
		return
	}
	b.markLlamaCppPortReady()
	b.repushPriority("llamacpp")
}
