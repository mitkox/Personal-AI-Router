// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestEngineModelsDir pins the per-engine {models_dir} default. The llama.cpp
// default must stay distinct from LM Studio's: the two engines serve different
// file layouts, and sharing a directory would make one engine's delete_model
// able to remove the other's files.
func TestEngineModelsDir(t *testing.T) {
	if got := engineModelsDir("lmstudio"); got != lmstudioModelsDir() {
		t.Errorf("engineModelsDir(lmstudio) = %q, want %q", got, lmstudioModelsDir())
	}
	if got := engineModelsDir("llamacpp"); got != llamacppModelsDir() {
		t.Errorf("engineModelsDir(llamacpp) = %q, want %q", got, llamacppModelsDir())
	}
	if engineModelsDir("llamacpp") == engineModelsDir("lmstudio") {
		t.Error("llamacpp and lmstudio share a models dir; their delete_model scopes would overlap")
	}
	if got := engineModelsDir("ollama"); got != "" {
		t.Errorf("engineModelsDir(ollama) = %q, want empty (Ollama manages its own store)", got)
	}
}

// TestLlamacppModelActionWire pins the ec load/unload/delete mapping for
// llama.cpp: the default load_model/unload_model/delete_model HTTP actions
// with the {model} alias param (not Ollama's run_model/name shape).
func TestLlamacppModelActionWire(t *testing.T) {
	cases := []struct {
		op     string
		action string
		key    string
	}{
		{"load", "load_model", "model"},
		{"unload", "unload_model", "model"},
		{"delete", "delete_model", "model"},
	}
	for _, c := range cases {
		action, params, err := modelActionWire("llamacpp", c.op, "test-model")
		if err != nil {
			t.Fatalf("modelActionWire(llamacpp, %q): %v", c.op, err)
		}
		if action != c.action {
			t.Errorf("modelActionWire(llamacpp, %q) action = %q, want %q", c.op, action, c.action)
		}
		var pm map[string]string
		if err := json.Unmarshal(params, &pm); err != nil {
			t.Fatalf("modelActionWire(llamacpp, %q) params not JSON: %v", c.op, err)
		}
		if pm[c.key] != "test-model" {
			t.Errorf("modelActionWire(llamacpp, %q) params = %v, want %q=test-model", c.op, pm, c.key)
		}
	}
}

// TestRunRemovePathActionLlamacppRestoresGGUF pins the alias-to-file mapping:
// router model ids strip the .gguf suffix, so deleting alias "test-model"
// must remove test-model.gguf under the models dir.
func TestRunRemovePathActionLlamacppRestoresGGUF(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := llamacppModelsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "test-model.gguf")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := &Executor{}
	act := Action{RemovePath: &ActionRemovePath{Root: "{models_dir}", Path: "{models_dir}/{model}"}}
	params := json.RawMessage(`{"model":"test-model"}`)
	if _, err := ex.runRemovePathAction(t.Context(), &engineState{}, "llamacpp", act, params); err != nil {
		t.Fatalf("runRemovePathAction: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target still exists after delete: %v", err)
	}
}
