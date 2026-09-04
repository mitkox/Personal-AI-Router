// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"strings"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
)

// llamacppproxy.go is the broker's llama.cpp counterpart to its ollama-proxy
// wiring (proxy.go / the proxy:* handlers in broker.go). llamacpp-proxy speaks
// the exact same JSON-RPC control plane as ollama-proxy (a "ready" port
// notification, node/add-manual/remove-manual, nodes/list, node/select, the
// workload:* lifecycle stream), so it reuses the proxyProcess client type;
// only the namespace differs — the broker relays it under llamacpp-proxy:
// instead of proxy:. Owning it here is what lets the broker bridge a reachable
// manual llama.cpp node into routing, the same way it does for Ollama.

func (b *Broker) setLlamaCppProxy(p *proxyProcess) {
	b.workersMu.Lock()
	b.llamacppProxy = p
	b.workersMu.Unlock()
}

func (b *Broker) getLlamaCppProxy() *proxyProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.llamacppProxy
}

func (b *Broker) finishLlamaCppProxyTerminal() {
	b.managedLlamaCppFacade.Store(false)
	b.markLlamaCppPortReady()
}

func (b *Broker) configureLlamaCppProxySupervisorCallbacks(sup *supervisor) {
	sup.onCrash, sup.onRecovered = b.supervisedWorkerCallbacks("llamacpp-proxy", func() { b.setLlamaCppProxy(nil) })
	sup.onExhausted = func(attempt int) {
		slog.Warn("llamacpp-proxy is terminally unavailable; releasing ownership gate", "attempt", attempt)
		b.finishLlamaCppProxyTerminal()
	}
}

func (b *Broker) llamacppProxyArgs() []string {
	var args []string
	if port := int(b.llamacppProxyStartupPort.Load()); port != 0 {
		args = []string{"--port", fmt.Sprintf("%d", port), "--ignore-persisted-port"}
	}
	return append(args, b.clusterDirArgs()...)
}

// spawnLlamaCppProxy is the llamacpp-proxy supervisor's spawn closure,
// mirroring spawnProxy. It reuses startProxy because the two proxies share a
// binary protocol.
func (b *Broker) spawnLlamaCppProxy() (supervisedHandle, error) {
	b.llamacppReadyMu.Lock()
	generation := b.llamacppProxyGeneration.Add(1)
	b.llamacppReadyMu.Unlock()
	// Thread the cluster dir so the llama.cpp proxy brings up its pin-gated LAN
	// mTLS ingress (and dials peers over mTLS) once this node is clustered.
	pp, err := startProxy(
		"llamacpp-proxy",
		b.llamacppProxyPath,
		applog.LevelString(),
		b.relayDir,
		func(method string, params json.RawMessage) {
			b.forwardLlamaCppProxyNotificationForGeneration(generation, method, params)
		},
		b.llamacppProxyArgs()...,
	)
	if err != nil {
		return nil, err
	}
	b.setLlamaCppProxy(pp)
	b.llamacppProxyPublishedGeneration.Store(generation)
	// A fast child can announce ready before its handle is published. Replay
	// reconciliation after publication; the gate's sync.Once makes this safe
	// when the notification goroutine already handled it.
	if ready, port := pp.Status(); ready && port > 0 {
		go b.reconcileLlamaCppProxyPortOnReadyForGeneration(generation, port)
	}
	slog.Info("llamacpp-proxy started", "path", b.llamacppProxyPath, "pid", pp.cmd.Process.Pid)
	return pp, nil
}

// forwardLlamaCppProxyNotification is the hook startProxy invokes on the
// llamacpp-proxy reader goroutine. It mirrors forwardProxyNotification:
// errors:report / errors:clear go into the nvpair-errors pipeline; workload
// lifecycle events are stamped and forwarded to the workload-manager for
// cluster broadcast (llamacpp-proxy tags its workloads "llamacpp"); everything
// else is re-emitted to llamacpp-proxy:subscribe'd clients as
// llamacpp-proxy:<method>. Readiness reconciliation runs on its own goroutine
// because its set-port/local-backend calls round-trip through this reader.
func (b *Broker) forwardLlamaCppProxyNotification(method string, params json.RawMessage) {
	b.forwardLlamaCppProxyNotificationForGeneration(b.llamacppProxyGeneration.Load(), method, params)
}

func (b *Broker) forwardLlamaCppProxyNotificationForGeneration(generation uint64, method string, params json.RawMessage) {
	if b.llamacppProxyGeneration.Load() != generation {
		return
	}
	if b.dispatchErrorsNotif("llamacpp-proxy", method, params) {
		return
	}
	// A process can win :8080 after preparation's free-port check but before
	// the proxy binds. The failed process is exiting, so set the next spawn to
	// an explicit fallback without calling back into this reader goroutine.
	if method == "error" {
		var ep struct {
			Code string `json:"code"`
			Port int    `json:"port"`
		}
		if json.Unmarshal(params, &ep) == nil && ep.Code == "bind-failed" {
			if b.managedLlamaCppFacade.Load() && ep.Port == managedLlamaCppFacadePort {
				_, _ = b.blockManagedLlamaCppFacade("another process acquired the compatibility port during startup", nil)
			} else {
				fallback := b.setLlamaCppProxyFallback(ep.Port)
				slog.Warn("llama.cpp proxy bind failed; retrying on fallback", "port", ep.Port, "fallback", fallback)
			}
		}
	}
	if proxyWorkloadMethods[method] {
		b.routeProxyWorkload(method, params)
		return
	}
	if method == noderec.NotifyNodeActivity {
		b.routeNodeActivity(params)
		return
	}
	if method == "ready" {
		var rp proxyReadyParams
		if err := json.Unmarshal(params, &rp); err == nil && rp.Port > 0 {
			go b.reconcileLlamaCppProxyPortOnReadyForGeneration(generation, rp.Port)
		}
	}
	b.proxyMu.Lock()
	subscribed := b.llamacppProxySubscribed
	b.proxyMu.Unlock()
	if !subscribed {
		return
	}
	if err := b.codec.Notify("llamacpp-proxy:"+method, params); err != nil {
		slog.Warn("forward llamacpp-proxy notification failed", "method", method, "err", err)
	}
}

// relayToLlamaCppProxy forwards an llamacpp-proxy:<method> request to
// llamacpp-proxy as <method> (prefix stripped) and maps its response straight
// back, mirroring relayToProxy. llamacpp-proxy:shutdown is refused — the broker
// owns the proxy's lifecycle.
func (b *Broker) relayToLlamaCppProxy(msg *Message) {
	method := strings.TrimPrefix(msg.Method, "llamacpp-proxy:")
	if method == "shutdown" {
		if err := b.codec.RespondError(msg.ID, -32601, "llamacpp-proxy:shutdown is not allowed; the broker owns the proxy lifecycle"); err != nil {
			log.Printf("failed to respond to llamacpp-proxy:shutdown: %v", err)
		}
		return
	}

	p := b.getLlamaCppProxy()
	if p == nil {
		if err := b.codec.RespondError(msg.ID, -32000, "llamacpp-proxy not available"); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
		return
	}

	result, rpcErr, err := p.Call(context.Background(), method, msg.Params)
	switch {
	case err != nil:
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("llamacpp-proxy call failed: %v", err)); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
	case rpcErr != nil:
		if err := b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message); err != nil {
			log.Printf("failed to relay llamacpp-proxy error for %s: %v", msg.Method, err)
		}
	default:
		if err := b.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to relay llamacpp-proxy result for %s: %v", msg.Method, err)
		}
	}
}
