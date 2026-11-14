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
