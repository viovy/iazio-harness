package agyhooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSanitizeHome_NoOpWhenGeminiConfigMissing(t *testing.T) {
	tmpHome := t.TempDir()
	res, err := SanitizeHome(tmpHome)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Changed || len(res.RemovedPluginDirs) != 0 || len(res.DisabledPlugins) != 0 {
		t.Fatalf("expected pure no-op, got %+v", res)
	}
	if _, statErr := os.Stat(filepath.Join(tmpHome, ".gemini")); !os.IsNotExist(statErr) {
		t.Fatalf("expected .gemini directory not to be created, got err=%v", statErr)
	}
}

func TestSanitizeHome_EmptyHomeInTestDoesNotTouchHost(t *testing.T) {
	res, err := SanitizeHome("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Changed {
		t.Fatalf("expected no-op under go test when homeDir is empty, got %+v", res)
	}
	resEnv, err := SanitizeEnv([]string{"PATH=/usr/bin"})
	if err != nil {
		t.Fatalf("unexpected SanitizeEnv error: %v", err)
	}
	if resEnv.Changed {
		t.Fatalf("expected SanitizeEnv no-op under go test without HOME, got %+v", resEnv)
	}
}

func TestSanitizeHome_PrunesBrokenPluginsPreservesMarketplaceAndPinsConfig0600(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := filepath.Join(tmpHome, ".gemini", "config")
	pluginsDir := filepath.Join(cfgDir, "plugins")

	// 1. Valid marketplace plugin with existing hook script (must remain untouched)
	dakHooksDir := filepath.Join(pluginsDir, "data-agent-kit-plugin", "hooks")
	if err := os.MkdirAll(dakHooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dakHooksDir, "telemetry_hook.js"), []byte("console.log('ok');\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dakHooksJSON := `{
  "dak-telemetry": {
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "node hooks/telemetry_hook.js --agent_name antigravity || exit 0"
          }
        ]
      }
    ]
  }
}`
	if err := os.WriteFile(filepath.Join(pluginsDir, "data-agent-kit-plugin", "hooks.json"), []byte(dakHooksJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Stale googlecloudtools.datacloud_telemetry directory (must be removed)
	dtDir := filepath.Join(pluginsDir, "googlecloudtools.datacloud_telemetry")
	if err := os.MkdirAll(dtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dtDir, "hooks.json"), []byte(`{"PreToolUse":[{"type":"command","command":"node hooks/missing.js"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. Another plugin with broken hooks.json referencing missing local scripts (must be removed)
	brokenDir := filepath.Join(pluginsDir, "broken-custom-plugin")
	if err := os.MkdirAll(brokenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "hooks.json"), []byte(`{"PreToolUse":[{"type":"command","command":"node scripts/does-not-exist.js"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 4. Dangling symlink (must be removed on Unix/macOS)
	danglingLink := filepath.Join(pluginsDir, "dangling-plugin")
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(tmpHome, "nonexistent-target"), danglingLink); err != nil {
			t.Fatal(err)
		}
	}

	// 5. Existing config.json with 0600 permissions, userSettings, marketplace metadata, and enabled datacloud_telemetry
	configPath := filepath.Join(cfgDir, "config.json")
	origConfig := `{
  "plugins": {
    "data-agent-kit-plugin": {
      "enabled": true,
      "installedFrom": {
        "id": "antigravity-plugins-official/plugin/data-agent-kit-plugin",
        "marketplace": "antigravity-plugins-official",
        "version": "1.2.0"
      }
    },
    "googlecloudtools.datacloud_telemetry": {
      "enabled": true,
      "customTag": "keep-me"
    },
    "broken-custom-plugin": {
      "enabled": true
    }
  },
  "userSettings": {
    "autoExecutionPolicy": "CASCADE_COMMANDS_AUTO_EXECUTION_EAGER",
    "customArray": [1, 2, 3]
  }
}`
	if err := os.WriteFile(configPath, []byte(origConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := SanitizeEnv([]string{"HOME=" + tmpHome})
	if err != nil {
		t.Fatalf("SanitizeEnv error: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected Changed=true")
	}

	if _, err := os.Stat(filepath.Join(pluginsDir, "data-agent-kit-plugin")); err != nil {
		t.Fatalf("expected valid marketplace plugin to remain, got err=%v", err)
	}
	if _, err := os.Stat(dtDir); !os.IsNotExist(err) {
		t.Fatalf("expected datacloud_telemetry dir removed, got err=%v", err)
	}
	if _, err := os.Stat(brokenDir); !os.IsNotExist(err) {
		t.Fatalf("expected broken-custom-plugin dir removed, got err=%v", err)
	}
	if runtime.GOOS != "windows" {
		if _, err := os.Lstat(danglingLink); !os.IsNotExist(err) {
			t.Fatalf("expected dangling symlink removed, got err=%v", err)
		}
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("expected config.json mode 0600, got %o", info.Mode().Perm())
	}

	updatedRaw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var updated map[string]any
	if err := json.Unmarshal(updatedRaw, &updated); err != nil {
		t.Fatal(err)
	}
	plugins := updated["plugins"].(map[string]any)
	dt := plugins["googlecloudtools.datacloud_telemetry"].(map[string]any)
	if en, ok := dt["enabled"].(bool); !ok || en {
		t.Fatalf("expected datacloud_telemetry enabled=false, got %#v", dt)
	}
	if dt["customTag"] != "keep-me" {
		t.Fatalf("expected extra fields on datacloud_telemetry preserved, got %#v", dt)
	}
	brokenCfg := plugins["broken-custom-plugin"].(map[string]any)
	if en, ok := brokenCfg["enabled"].(bool); !ok || en {
		t.Fatalf("expected missing non-marketplace plugin disabled, got %#v", brokenCfg)
	}
	dak := plugins["data-agent-kit-plugin"].(map[string]any)
	if en, ok := dak["enabled"].(bool); !ok || !en {
		t.Fatalf("expected valid marketplace plugin to remain enabled, got %#v", dak)
	}
	us := updated["userSettings"].(map[string]any)
	if us["autoExecutionPolicy"] != "CASCADE_COMMANDS_AUTO_EXECUTION_EAGER" {
		t.Fatalf("expected userSettings preserved verbatim, got %#v", us)
	}

	// Second call must be a clean idempotent no-op
	res2, err := SanitizeHome(tmpHome)
	if err != nil {
		t.Fatalf("second SanitizeHome error: %v", err)
	}
	if res2.Changed {
		t.Fatalf("expected second SanitizeHome to be idempotent (Changed=false), got %+v", res2)
	}
}

func TestDetectAndScanTranscriptSliceForStaleHook(t *testing.T) {
	errSample := `JSON hook "jsonhook__googlecloudtools.datacloud_telemetry_PreToolUse_0_0" failed:
command failed: chdir /Users/romeo/.gemini/config/plugins/googlecloudtools.datacloud_telemetry: no such file or directory`

	matched, plugin := DetectStalePluginHookFailure(errSample)
	if !matched || plugin != "googlecloudtools.datacloud_telemetry" {
		t.Fatalf("expected match for datacloud_telemetry, got matched=%v plugin=%q", matched, plugin)
	}

	if m, _ := DetectStalePluginHookFailure("normal output without hook failure"); m {
		t.Fatal("unexpected match for normal text")
	}

	tp := filepath.Join(t.TempDir(), "transcript.jsonl")
	userPromptLine := `{"step_index":0,"type":"USER_INPUT","status":"DONE","content":"Please fix ` + strings.ReplaceAll(strings.ReplaceAll(errSample, `"`, `\"`), "\n", `\n`) + `"}` + "\n"
	toolRespLine := `{"step_index":1,"type":"TOOL_RESPONSE","status":"ERROR","content":"` + strings.ReplaceAll(strings.ReplaceAll(errSample, `"`, `\"`), "\n", `\n`) + `"}` + "\n"
	if err := os.WriteFile(tp, []byte(userPromptLine), 0o644); err != nil {
		t.Fatal(err)
	}
	// USER_INPUT alone must NOT trigger
	if m, p := ScanTranscriptSliceForStaleHook(tp, 0); m {
		t.Fatalf("expected USER_INPUT not to trigger, got matched=%v plugin=%q", m, p)
	}

	// Appending TOOL_RESPONSE triggers when scanning from 0
	f, err := os.OpenFile(tp, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(toolRespLine); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	m, p := ScanTranscriptSliceForStaleHook(tp, 0)
	if !m || p != "googlecloudtools.datacloud_telemetry" {
		t.Fatalf("expected TOOL_RESPONSE slice to match, got matched=%v plugin=%q", m, p)
	}

	// Turn 2 slice starting at Turn 1 EOF must NOT re-trigger on Turn 1 error
	fi, err := os.Stat(tp)
	if err != nil {
		t.Fatal(err)
	}
	turn2Offset := fi.Size()
	f2, err := os.OpenFile(tp, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.WriteString(`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"All clear after sanitize."}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f2.Close()

	if m2, p2 := ScanTranscriptSliceForStaleHook(tp, turn2Offset); m2 {
		t.Fatalf("expected Turn 2 transcript slice not to re-trigger on Turn 1 error, got matched=%v plugin=%q", m2, p2)
	}
}

func TestSanitizeEnv_UserProfileAndPluginEdgeCases(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := filepath.Join(tmpHome, ".gemini", "config")
	pluginsDir := filepath.Join(cfgDir, "plugins")

	// Valid plugin whose command uses quoted path with shell operator and an existing script
	validDir := filepath.Join(pluginsDir, "quoted-valid-plugin")
	if err := os.MkdirAll(filepath.Join(validDir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(validDir, "scripts", "ok.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	validHooks := `{"PreToolUse":[{"hooks":[{"command":"sh 'scripts/ok.sh' && echo ok"}]}]}`
	if err := os.WriteFile(filepath.Join(validDir, "hooks.json"), []byte(validHooks), 0o644); err != nil {
		t.Fatal(err)
	}

	// Broken plugin whose relative script target is missing
	brokenQuotedDir := filepath.Join(pluginsDir, "broken-quoted-plugin")
	if err := os.MkdirAll(brokenQuotedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	brokenHooks := `{"PreToolUse":[{"command":"python3 \"scripts/missing.py\"; echo fallback"}]}`
	if err := os.WriteFile(filepath.Join(brokenQuotedDir, "hooks.json"), []byte(brokenHooks), 0o644); err != nil {
		t.Fatal(err)
	}

	// Plugin with malformed hooks.json should be preserved safely
	badJSONPlugin := filepath.Join(pluginsDir, "bad-hooks-json-plugin")
	if err := os.MkdirAll(badJSONPlugin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badJSONPlugin, "hooks.json"), []byte(`{not-json`), 0o644); err != nil {
		t.Fatal(err)
	}

	// config.json with no "plugins" section initially
	cfgPath := filepath.Join(cfgDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Extract home from USERPROFILE when HOME is empty; ignore blank/malformed env pairs
	res, err := SanitizeEnv([]string{"", "INVALID_PAIR", "HOME=   ", "USERPROFILE=" + tmpHome})
	if err != nil {
		t.Fatalf("SanitizeEnv error: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected USERPROFILE sanitization to mutate")
	}
	if _, err := os.Stat(validDir); err != nil {
		t.Fatalf("expected valid quoted plugin preserved, got %v", err)
	}
	if _, err := os.Stat(badJSONPlugin); err != nil {
		t.Fatalf("expected invalid hooks.json plugin preserved conservatively, got %v", err)
	}
	if _, err := os.Stat(brokenQuotedDir); !os.IsNotExist(err) {
		t.Fatalf("expected broken quoted plugin removed, got %v", err)
	}

	// Verify malformed config.json surfaces an error
	if err := os.WriteFile(cfgPath, []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SanitizeHome(tmpHome); err == nil {
		t.Fatal("expected error on invalid config.json")
	}
}

func TestDetectAndScanTranscriptSliceForStaleHook_EdgeCases(t *testing.T) {
	if m, _ := ScanTranscriptSliceForStaleHook("", 0); m {
		t.Fatal("expected false for empty transcript path")
	}
	if m, _ := ScanTranscriptSliceForStaleHook(filepath.Join(t.TempDir(), "missing.jsonl"), 0); m {
		t.Fatal("expected false for missing transcript file")
	}

	chdirOnly := `command failed: chdir /Users/romeo/.gemini/config/plugins/googlecloudtools.datacloud_telemetry: no such file or directory`
	if m, p := DetectStalePluginHookFailure("", "   ", chdirOnly); !m || p != "googlecloudtools.datacloud_telemetry" {
		t.Fatalf("expected chdir pattern match, got m=%v p=%q", m, p)
	}

	tp := filepath.Join(t.TempDir(), "transcript.jsonl")
	nestedLine := `{"step_index":0,"type":"PLANNER_RESPONSE","nested":{"items":["ok","command failed: chdir /Users/romeo/.gemini/config/plugins/custom.plugin: no such file or directory"]}}` + "\n"
	if err := os.WriteFile(tp, []byte(nestedLine), 0o644); err != nil {
		t.Fatal(err)
	}
	// Negative startOffset and past-EOF startOffset both clamp/reset cleanly to 0
	if m, p := ScanTranscriptSliceForStaleHook(tp, -10); !m || p != "custom.plugin" {
		t.Fatalf("expected negative offset reset to detect custom.plugin, got m=%v p=%q", m, p)
	}
	if m, p := ScanTranscriptSliceForStaleHook(tp, 999999); !m || p != "custom.plugin" {
		t.Fatalf("expected past-EOF offset reset to detect custom.plugin, got m=%v p=%q", m, p)
	}

	// Raw malformed JSONL line fallback
	rawPath := filepath.Join(t.TempDir(), "raw.jsonl")
	if err := os.WriteFile(rawPath, []byte("not-json\n"+chdirOnly+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, p := ScanTranscriptSliceForStaleHook(rawPath, 0); !m || p != "googlecloudtools.datacloud_telemetry" {
		t.Fatalf("expected raw fallback match, got m=%v p=%q", m, p)
	}

	// Unreadable transcript path (directory instead of regular file)
	if m, _ := ScanTranscriptSliceForStaleHook(t.TempDir(), 0); m {
		t.Fatal("expected false when transcript path is a directory")
	}

	// Primary JSON hook regex without full chdir suffix line, plus whitespace-only inputs
	hookQuoteOnly := `JSON hook "jsonhook__foo.bar_PreToolUse_0_0" failed: chdir /missing: no such file or directory`
	if m, p := DetectStalePluginHookFailure("", "  ", hookQuoteOnly); !m || p != "foo.bar" {
		t.Fatalf("expected foo.bar from primary hook regex, got m=%v p=%q", m, p)
	}
	if m, _ := DetectStalePluginHookFailure("", "   "); m {
		t.Fatal("expected false for empty/whitespace texts")
	}
}

func TestSanitizeHome_CreatesConfigWhenMissingAndEdgeBranches(t *testing.T) {
	tmpHome := t.TempDir()
	cfgDir := filepath.Join(tmpHome, ".gemini", "config")
	pluginsDir := filepath.Join(cfgDir, "plugins")
	dtDir := filepath.Join(pluginsDir, "googlecloudtools.datacloud_telemetry")
	if err := os.MkdirAll(dtDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Non-directory entry in plugins/, empty hooks.json command list, shell pipe/|| cutoffs,
	// escaping relative path, bare relative .js path, and abs path inside pluginDir
	if err := os.WriteFile(filepath.Join(pluginsDir, "README.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	noCmdDir := filepath.Join(pluginsDir, "empty-cmds-plugin")
	if err := os.MkdirAll(noCmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(noCmdDir, "hooks.json"), []byte(`{"PreToolUse":[]}`), 0o644)

	escapeDir := filepath.Join(pluginsDir, "escape-path-plugin")
	_ = os.MkdirAll(escapeDir, 0o755)
	_ = os.WriteFile(filepath.Join(escapeDir, "hooks.json"), []byte(`{"PreToolUse":[{"command":"node ../outside.js || echo ok"}]}`), 0o644)

	pipeOkDir := filepath.Join(pluginsDir, "pipe-ok-plugin")
	_ = os.MkdirAll(pipeOkDir, 0o755)
	_ = os.WriteFile(filepath.Join(pipeOkDir, "run.js"), []byte("console.log(1);\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pipeOkDir, "hooks.json"), []byte(`{"PreToolUse":[{"command":"node run.js | cat"}]}`), 0o644)

	absBrokenDir := filepath.Join(pluginsDir, "abs-broken-plugin")
	_ = os.MkdirAll(absBrokenDir, 0o755)
	missingAbs := filepath.Join(absBrokenDir, "hooks", "missing.js")
	_ = os.WriteFile(filepath.Join(absBrokenDir, "hooks.json"), []byte(`{"PreToolUse":[{"command":"node \"`+filepath.ToSlash(missingAbs)+`\""}]}`), 0o644)

	// config.json does NOT exist initially -> SanitizeHome creates it with mode 0600 and datacloud_telemetry disabled
	res, err := SanitizeHome(tmpHome)
	if err != nil {
		t.Fatalf("SanitizeHome error: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected Changed=true when creating missing config.json after removing datacloud_telemetry")
	}
	cfgPath := filepath.Join(cfgDir, "config.json")
	cfgRaw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("expected created config.json, got err=%v", err)
	}
	if !strings.Contains(string(cfgRaw), `"enabled": false`) {
		t.Fatalf("expected datacloud_telemetry enabled=false in created config.json, got:\n%s", string(cfgRaw))
	}
	if _, err := os.Stat(absBrokenDir); !os.IsNotExist(err) {
		t.Fatalf("expected abs-broken-plugin removed, got %v", err)
	}
	if _, err := os.Stat(pipeOkDir); err != nil {
		t.Fatalf("expected pipe-ok-plugin preserved, got %v", err)
	}
	if _, err := os.Stat(escapeDir); err != nil {
		t.Fatalf("expected escape-path-plugin preserved, got %v", err)
	}
	if _, err := os.Stat(noCmdDir); err != nil {
		t.Fatalf("expected empty-cmds-plugin preserved, got %v", err)
	}

	// Non-map plugin entry inside config.json plugins map is ignored without failing
	_ = os.WriteFile(cfgPath, []byte(`{"plugins":{"nonMapValue":"ignored","googlecloudtools.datacloud_telemetry":{"enabled":false}}}`), 0o600)
	res2, err := SanitizeHome(tmpHome)
	if err != nil {
		t.Fatalf("SanitizeHome with non-map plugin entry failed: %v", err)
	}
	if res2.Changed {
		t.Fatalf("expected idempotent no-op, got %+v", res2)
	}
}
