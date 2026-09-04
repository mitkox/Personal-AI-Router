// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"testing"

	"nvpair-tui/rpc"
)

// TestProxiesViewListsThreeEngines pins the Proxies tab to all broker-fronted
// reverse proxies. A fourth proxy added without a row here would silently miss
// status, node lists, selection, and port actions.
func TestProxiesViewListsThreeEngines(t *testing.T) {
	v := newProxiesView(nil)
	want := []struct{ label, prefix string }{
		{"Ollama", "proxy"},
		{"LM Studio", "lmstudio-proxy"},
		{"llama.cpp", "llamacpp-proxy"},
	}
	if len(v.engines) != len(want) {
		t.Fatalf("engines = %d, want %d", len(v.engines), len(want))
	}
	for i, w := range want {
		if v.engines[i].label != w.label || v.engines[i].prefix != w.prefix {
			t.Errorf("engines[%d] = {%q,%q}, want {%q,%q}",
				i, v.engines[i].label, v.engines[i].prefix, w.label, w.prefix)
		}
	}
}

// TestProxiesNotificationRoutesByPrefix pins notification demux for the third
// proxy: a llamacpp-proxy:ready marks the llama.cpp row ready on its port,
// and unknown namespaces are ignored.
func TestProxiesNotificationRoutesByPrefix(t *testing.T) {
	v := newProxiesView(nil)
	v.handleNotification(&rpc.Message{
		Method: "llamacpp-proxy:ready",
		Params: json.RawMessage(`{"port":8080}`),
	})
	if !v.engines[2].ready || v.engines[2].port != 8080 {
		t.Fatalf("llamacpp row = ready=%v port=%d, want ready=true port=8080",
			v.engines[2].ready, v.engines[2].port)
	}
	if v.engines[0].ready || v.engines[1].ready {
		t.Fatal("llamacpp-proxy:ready leaked onto another engine row")
	}
	v.handleNotification(&rpc.Message{Method: "unknown:ready"})
	if v.engines[0].ready || v.engines[1].ready {
		t.Fatal("unknown namespace changed engine rows")
	}
}
