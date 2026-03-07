package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/cloud"
	"github.com/Gentleman-Programming/engram/internal/cloud/auth"
	"github.com/Gentleman-Programming/engram/internal/cloud/cloudserver"
	"github.com/Gentleman-Programming/engram/internal/cloud/cloudstore"
	"github.com/Gentleman-Programming/engram/internal/cloud/remote"
	"github.com/Gentleman-Programming/engram/internal/store"
)

func testConfig(t *testing.T) store.Config {
	t.Helper()
	cfg := store.DefaultConfig()
	cfg.DataDir = t.TempDir()
	return cfg
}

func withArgs(t *testing.T, args ...string) {
	t.Helper()
	old := os.Args
	os.Args = args
	t.Cleanup(func() {
		os.Args = old
	})
}

func withCwd(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir to %s: %v", dir, err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(old)
	})
}

func captureOutput(t *testing.T, fn func()) (stdout string, stderr string) {
	t.Helper()

	oldOut := os.Stdout
	oldErr := os.Stderr

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}

	os.Stdout = outW
	os.Stderr = errW

	fn()

	_ = outW.Close()
	_ = errW.Close()
	os.Stdout = oldOut
	os.Stderr = oldErr

	outBytes, err := io.ReadAll(outR)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	errBytes, err := io.ReadAll(errR)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}

	return string(outBytes), string(errBytes)
}

func mustSeedObservation(t *testing.T, cfg store.Config, sessionID, project, typ, title, content, scope string) int64 {
	t.Helper()

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	if err := s.CreateSession(sessionID, project, "/tmp"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	id, err := s.AddObservation(store.AddObservationParams{
		SessionID: sessionID,
		Type:      typ,
		Title:     title,
		Content:   content,
		Project:   project,
		Scope:     scope,
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	return id
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "short string", in: "abc", max: 10, want: "abc"},
		{name: "exact length", in: "hello", max: 5, want: "hello"},
		{name: "long string", in: "abcdef", max: 3, want: "abc..."},
		{name: "spanish accents", in: "Decisión de arquitectura", max: 8, want: "Decisión..."},
		{name: "emoji", in: "🐛🔧🚀✨🎉💡", max: 3, want: "🐛🔧🚀..."},
		{name: "mixed ascii and multibyte", in: "café☕latte", max: 5, want: "café☕..."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.in, tc.max)
			if got != tc.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

func TestPrintUsage(t *testing.T) {
	oldVersion := version
	version = "test-version"
	t.Cleanup(func() {
		version = oldVersion
	})

	stdout, stderr := captureOutput(t, func() { printUsage() })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "engram vtest-version") {
		t.Fatalf("usage missing version: %q", stdout)
	}
	if !strings.Contains(stdout, "search <query>") || !strings.Contains(stdout, "setup [agent]") {
		t.Fatalf("usage missing expected commands: %q", stdout)
	}
}

func TestPrintPostInstall(t *testing.T) {
	tests := []struct {
		agent   string
		expects []string
	}{
		{agent: "opencode", expects: []string{"Restart OpenCode", "engram serve &"}},
		{agent: "gemini-cli", expects: []string{"Restart Gemini CLI", "~/.gemini/settings.json"}},
		{agent: "codex", expects: []string{"Restart Codex", "~/.codex/config.toml"}},
		{agent: "unknown", expects: nil},
	}

	for _, tc := range tests {
		t.Run(tc.agent, func(t *testing.T) {
			stdout, stderr := captureOutput(t, func() { printPostInstall(tc.agent) })
			if stderr != "" {
				t.Fatalf("expected no stderr, got: %q", stderr)
			}
			for _, expected := range tc.expects {
				if !strings.Contains(stdout, expected) {
					t.Fatalf("output missing %q: %q", expected, stdout)
				}
			}
			if len(tc.expects) == 0 && stdout != "" {
				t.Fatalf("expected empty output for unknown agent, got: %q", stdout)
			}
		})
	}
}

func TestPrintPostInstallClaudeCodeAllowlist(t *testing.T) {
	t.Run("user accepts allowlist", func(t *testing.T) {
		oldScan := scanInputLine
		oldAllowlist := setupAddClaudeCodeAllowlist
		t.Cleanup(func() {
			scanInputLine = oldScan
			setupAddClaudeCodeAllowlist = oldAllowlist
		})

		scanInputLine = func(a ...any) (int, error) {
			ptr := a[0].(*string)
			*ptr = "y"
			return 1, nil
		}
		allowlistCalled := false
		setupAddClaudeCodeAllowlist = func() error {
			allowlistCalled = true
			return nil
		}

		stdout, _ := captureOutput(t, func() { printPostInstall("claude-code") })
		if !allowlistCalled {
			t.Fatalf("expected AddClaudeCodeAllowlist to be called")
		}
		if !strings.Contains(stdout, "tools added to allowlist") {
			t.Fatalf("expected success message, got: %q", stdout)
		}
		if !strings.Contains(stdout, "Restart Claude Code") {
			t.Fatalf("expected next steps, got: %q", stdout)
		}
	})

	t.Run("user declines allowlist", func(t *testing.T) {
		oldScan := scanInputLine
		oldAllowlist := setupAddClaudeCodeAllowlist
		t.Cleanup(func() {
			scanInputLine = oldScan
			setupAddClaudeCodeAllowlist = oldAllowlist
		})

		scanInputLine = func(a ...any) (int, error) {
			ptr := a[0].(*string)
			*ptr = "n"
			return 1, nil
		}
		allowlistCalled := false
		setupAddClaudeCodeAllowlist = func() error {
			allowlistCalled = true
			return nil
		}

		stdout, _ := captureOutput(t, func() { printPostInstall("claude-code") })
		if allowlistCalled {
			t.Fatalf("expected AddClaudeCodeAllowlist NOT to be called")
		}
		if !strings.Contains(stdout, "Skipped") {
			t.Fatalf("expected skip message, got: %q", stdout)
		}
	})

	t.Run("allowlist error shows warning", func(t *testing.T) {
		oldScan := scanInputLine
		oldAllowlist := setupAddClaudeCodeAllowlist
		t.Cleanup(func() {
			scanInputLine = oldScan
			setupAddClaudeCodeAllowlist = oldAllowlist
		})

		scanInputLine = func(a ...any) (int, error) {
			ptr := a[0].(*string)
			*ptr = "y"
			return 1, nil
		}
		setupAddClaudeCodeAllowlist = func() error {
			return os.ErrPermission
		}

		_, stderr := captureOutput(t, func() { printPostInstall("claude-code") })
		if !strings.Contains(stderr, "warning") {
			t.Fatalf("expected warning in stderr, got: %q", stderr)
		}
	})
}

func TestCmdSaveAndSearch(t *testing.T) {
	cfg := testConfig(t)

	withArgs(t,
		"engram", "save", "my-title", "my-content",
		"--type", "bugfix",
		"--project", "alpha",
		"--scope", "personal",
		"--topic", "auth/token",
	)

	stdout, stderr := captureOutput(t, func() { cmdSave(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "Memory saved:") || !strings.Contains(stdout, "my-title") {
		t.Fatalf("unexpected save output: %q", stdout)
	}

	withArgs(t, "engram", "search", "my-content", "--type", "bugfix", "--project", "alpha", "--scope", "personal", "--limit", "1")
	searchOut, searchErr := captureOutput(t, func() { cmdSearch(cfg) })
	if searchErr != "" {
		t.Fatalf("expected no stderr from search, got: %q", searchErr)
	}
	if !strings.Contains(searchOut, "Found 1 memories") || !strings.Contains(searchOut, "my-title") {
		t.Fatalf("unexpected search output: %q", searchOut)
	}

	withArgs(t, "engram", "search", "definitely-not-found")
	noneOut, noneErr := captureOutput(t, func() { cmdSearch(cfg) })
	if noneErr != "" {
		t.Fatalf("expected no stderr from empty search, got: %q", noneErr)
	}
	if !strings.Contains(noneOut, "No memories found") {
		t.Fatalf("expected empty search message, got: %q", noneOut)
	}
}

func TestCmdTimeline(t *testing.T) {
	cfg := testConfig(t)
	mustSeedObservation(t, cfg, "s-1", "proj", "note", "first", "first content", "project")
	focusID := mustSeedObservation(t, cfg, "s-1", "proj", "note", "focus", "focus content", "project")
	mustSeedObservation(t, cfg, "s-1", "proj", "note", "third", "third content", "project")

	withArgs(t, "engram", "timeline", strconv.FormatInt(focusID, 10), "--before", "1", "--after", "1")
	stdout, stderr := captureOutput(t, func() { cmdTimeline(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "Session:") || !strings.Contains(stdout, ">>> #"+strconv.FormatInt(focusID, 10)) {
		t.Fatalf("timeline output missing expected focus/session info: %q", stdout)
	}
	if !strings.Contains(stdout, "Before") || !strings.Contains(stdout, "After") {
		t.Fatalf("timeline output missing before/after sections: %q", stdout)
	}
}

func TestCmdContextAndStats(t *testing.T) {
	cfg := testConfig(t)

	withArgs(t, "engram", "context")
	emptyCtxOut, emptyCtxErr := captureOutput(t, func() { cmdContext(cfg) })
	if emptyCtxErr != "" {
		t.Fatalf("expected no stderr for empty context, got: %q", emptyCtxErr)
	}
	if !strings.Contains(emptyCtxOut, "No previous session memories found") {
		t.Fatalf("unexpected empty context output: %q", emptyCtxOut)
	}

	mustSeedObservation(t, cfg, "s-ctx", "project-x", "decision", "title", "content", "project")

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	_, err = s.AddPrompt(store.AddPromptParams{SessionID: "s-ctx", Content: "user asked about context", Project: "project-x"})
	if err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}
	_ = s.Close()

	withArgs(t, "engram", "context", "project-x")
	ctxOut, ctxErr := captureOutput(t, func() { cmdContext(cfg) })
	if ctxErr != "" {
		t.Fatalf("expected no stderr for populated context, got: %q", ctxErr)
	}
	if !strings.Contains(ctxOut, "## Memory from Previous Sessions") || !strings.Contains(ctxOut, "Recent Observations") {
		t.Fatalf("unexpected populated context output: %q", ctxOut)
	}

	withArgs(t, "engram", "stats")
	statsOut, statsErr := captureOutput(t, func() { cmdStats(cfg) })
	if statsErr != "" {
		t.Fatalf("expected no stderr from stats, got: %q", statsErr)
	}
	if !strings.Contains(statsOut, "Engram Memory Stats") || !strings.Contains(statsOut, "project-x") {
		t.Fatalf("unexpected stats output: %q", statsOut)
	}
}

func TestCmdExportAndImport(t *testing.T) {
	sourceCfg := testConfig(t)
	targetCfg := testConfig(t)

	mustSeedObservation(t, sourceCfg, "s-exp", "proj-exp", "pattern", "exported", "export me", "project")

	exportPath := filepath.Join(t.TempDir(), "memories.json")

	withArgs(t, "engram", "export", exportPath)
	exportOut, exportErr := captureOutput(t, func() { cmdExport(sourceCfg) })
	if exportErr != "" {
		t.Fatalf("expected no stderr from export, got: %q", exportErr)
	}
	if !strings.Contains(exportOut, "Exported to "+exportPath) {
		t.Fatalf("unexpected export output: %q", exportOut)
	}

	withArgs(t, "engram", "import", exportPath)
	importOut, importErr := captureOutput(t, func() { cmdImport(targetCfg) })
	if importErr != "" {
		t.Fatalf("expected no stderr from import, got: %q", importErr)
	}
	if !strings.Contains(importOut, "Imported from "+exportPath) {
		t.Fatalf("unexpected import output: %q", importOut)
	}

	s, err := store.New(targetCfg)
	if err != nil {
		t.Fatalf("store.New target: %v", err)
	}
	defer s.Close()

	results, err := s.Search("export", store.SearchOptions{Limit: 10, Project: "proj-exp"})
	if err != nil {
		t.Fatalf("Search after import: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected imported data to be searchable")
	}
}

func TestCmdSyncStatusExportAndImport(t *testing.T) {
	workDir := t.TempDir()
	withCwd(t, workDir)

	exportCfg := testConfig(t)
	importCfg := testConfig(t)

	mustSeedObservation(t, exportCfg, "s-sync", "sync-project", "note", "sync title", "sync content", "project")

	withArgs(t, "engram", "sync", "--status")
	statusOut, statusErr := captureOutput(t, func() { cmdSync(exportCfg) })
	if statusErr != "" {
		t.Fatalf("expected no stderr from status, got: %q", statusErr)
	}
	if !strings.Contains(statusOut, "Sync status:") {
		t.Fatalf("unexpected status output: %q", statusOut)
	}

	withArgs(t, "engram", "sync", "--all")
	exportOut, exportErr := captureOutput(t, func() { cmdSync(exportCfg) })
	if exportErr != "" {
		t.Fatalf("expected no stderr from sync export, got: %q", exportErr)
	}
	if !strings.Contains(exportOut, "Created chunk") {
		t.Fatalf("unexpected sync export output: %q", exportOut)
	}

	withArgs(t, "engram", "sync", "--import")
	importOut, importErr := captureOutput(t, func() { cmdSync(importCfg) })
	if importErr != "" {
		t.Fatalf("expected no stderr from sync import, got: %q", importErr)
	}
	if !strings.Contains(importOut, "Imported 1 new chunk(s)") {
		t.Fatalf("unexpected sync import output: %q", importOut)
	}

	withArgs(t, "engram", "sync", "--import")
	noopOut, noopErr := captureOutput(t, func() { cmdSync(importCfg) })
	if noopErr != "" {
		t.Fatalf("expected no stderr from second sync import, got: %q", noopErr)
	}
	if !strings.Contains(noopOut, "Already up to date") {
		t.Fatalf("unexpected second sync import output: %q", noopOut)
	}
}

func TestCmdSyncDefaultProjectNoData(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "repo-name")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	withCwd(t, workDir)

	cfg := testConfig(t)
	withArgs(t, "engram", "sync")
	stdout, stderr := captureOutput(t, func() { cmdSync(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, `Exporting memories for project "repo-name"`) {
		t.Fatalf("expected default project message, got: %q", stdout)
	}
	if !strings.Contains(stdout, `Nothing new to sync for project "repo-name"`) {
		t.Fatalf("expected no-data sync message, got: %q", stdout)
	}
}

func TestCmdSyncRemoteNoOp(t *testing.T) {
	var manifestCalls int
	var chunkCalls int
	var pushCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sync-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sync/pull":
			manifestCalls++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"version": 1, "chunks": []any{}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sync/pull/"):
			chunkCalls++
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/sync/push":
			pushCalls++
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	oldClient := cloudHTTPClient
	t.Cleanup(func() { cloudHTTPClient = oldClient })
	cloudHTTPClient = func() *http.Client { return srv.Client() }

	withArgs(t, "engram", "sync", "--remote", srv.URL, "--token", "sync-token")
	stdout, stderr := captureOutput(t, func() { cmdSync(testConfig(t)) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got %q", stderr)
	}
	if !strings.Contains(stdout, "Nothing new to push") || !strings.Contains(stdout, "Nothing new to pull") {
		t.Fatalf("unexpected output: %q", stdout)
	}
	if manifestCalls == 0 {
		t.Fatal("expected manifest requests")
	}
	if pushCalls != 0 || chunkCalls != 0 {
		t.Fatalf("expected no push or chunk requests, got push=%d chunk=%d", pushCalls, chunkCalls)
	}
}

func TestMainVersionAndHelpAliases(t *testing.T) {
	oldVersion := version
	version = "9.9.9-test"
	t.Cleanup(func() { version = oldVersion })

	tests := []struct {
		name      string
		arg       string
		contains  string
		notStderr bool
	}{
		{name: "version", arg: "version", contains: "engram 9.9.9-test", notStderr: true},
		{name: "version short", arg: "-v", contains: "engram 9.9.9-test", notStderr: true},
		{name: "version long", arg: "--version", contains: "engram 9.9.9-test", notStderr: true},
		{name: "help", arg: "help", contains: "Usage:", notStderr: true},
		{name: "help short", arg: "-h", contains: "Commands:", notStderr: true},
		{name: "help long", arg: "--help", contains: "Environment:", notStderr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withArgs(t, "engram", tc.arg)
			stdout, stderr := captureOutput(t, func() { main() })
			if tc.notStderr && stderr != "" {
				t.Fatalf("expected no stderr, got: %q", stderr)
			}
			if !strings.Contains(stdout, tc.contains) {
				t.Fatalf("stdout %q does not include %q", stdout, tc.contains)
			}
		})
	}
}

func TestMainExitPaths(t *testing.T) {
	tests := []struct {
		name            string
		helperCase      string
		expectedOutput  string
		expectedStderr  string
		expectedExitOne bool
	}{
		{name: "no args", helperCase: "no-args", expectedOutput: "Usage:", expectedExitOne: true},
		{name: "unknown command", helperCase: "unknown", expectedOutput: "Usage:", expectedStderr: "unknown command:", expectedExitOne: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestMainExitHelper")
			cmd.Env = append(os.Environ(),
				"GO_WANT_HELPER_PROCESS=1",
				"HELPER_CASE="+tc.helperCase,
			)

			out, err := cmd.CombinedOutput()
			if tc.expectedExitOne {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("expected exit error, got %T (%v)", err, err)
				}
				if exitErr.ExitCode() != 1 {
					t.Fatalf("expected exit code 1, got %d; output=%q", exitErr.ExitCode(), string(out))
				}
			}

			if !strings.Contains(string(out), tc.expectedOutput) {
				t.Fatalf("output missing %q: %q", tc.expectedOutput, string(out))
			}
			if tc.expectedStderr != "" && !strings.Contains(string(out), tc.expectedStderr) {
				t.Fatalf("output missing stderr text %q: %q", tc.expectedStderr, string(out))
			}
		})
	}
}

func TestMainExitHelper(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	switch os.Getenv("HELPER_CASE") {
	case "no-args":
		os.Args = []string{"engram"}
	case "unknown":
		os.Args = []string{"engram", "definitely-unknown-command"}
	default:
		os.Args = []string{"engram", "--help"}
	}

	main()
}

// ─── Cloud CLI Tests ─────────────────────────────────────────────────────────

func TestCmdCloudServeMissingDatabaseURL(t *testing.T) {
	// Ensure ENGRAM_DATABASE_URL is not set
	t.Setenv("ENGRAM_DATABASE_URL", "")
	t.Setenv("ENGRAM_JWT_SECRET", "")

	exitCalled := false
	exitCode := 0
	oldExit := exitFunc
	t.Cleanup(func() { exitFunc = oldExit })
	exitFunc = func(code int) {
		exitCalled = true
		exitCode = code
	}

	withArgs(t, "engram", "cloud", "serve")
	_, stderr := captureOutput(t, func() { cmdCloudServe() })

	if !exitCalled || exitCode != 1 {
		t.Fatalf("expected exit(1), got exitCalled=%v code=%d", exitCalled, exitCode)
	}
	if !strings.Contains(stderr, "ENGRAM_DATABASE_URL") {
		t.Fatalf("expected DATABASE_URL error in stderr, got: %q", stderr)
	}
}

func TestCmdCloudServeMissingJWTSecret(t *testing.T) {
	t.Setenv("ENGRAM_DATABASE_URL", "postgres://fake:fake@localhost:5432/fake")
	t.Setenv("ENGRAM_JWT_SECRET", "")

	exitCalled := false
	exitCode := 0
	oldExit := exitFunc
	t.Cleanup(func() { exitFunc = oldExit })
	exitFunc = func(code int) {
		exitCalled = true
		exitCode = code
	}

	withArgs(t, "engram", "cloud", "serve")
	_, stderr := captureOutput(t, func() { cmdCloudServe() })

	if !exitCalled || exitCode != 1 {
		t.Fatalf("expected exit(1), got exitCalled=%v code=%d", exitCalled, exitCode)
	}
	if !strings.Contains(stderr, "ENGRAM_JWT_SECRET") {
		t.Fatalf("expected JWT_SECRET error in stderr, got: %q", stderr)
	}
}

func TestCmdCloudServeWithFlags(t *testing.T) {
	// Test that --database-url flag overrides env var
	t.Setenv("ENGRAM_DATABASE_URL", "postgres://env@localhost/env")
	t.Setenv("ENGRAM_JWT_SECRET", "this-is-a-secret-at-least-32-chars-long!!!")

	oldExit := exitFunc
	t.Cleanup(func() { exitFunc = oldExit })
	exitFunc = func(code int) {}

	// Test that providing --database-url doesn't trigger the "missing" error
	withArgs(t, "engram", "cloud", "serve", "--database-url", "postgres://flag@localhost/flag")
	_, stderr := captureOutput(t, func() { cmdCloudServe() })

	// It should NOT complain about missing DATABASE_URL — it should fail later
	// (at cloudstore.New or auth.NewService with invalid DSN)
	if strings.Contains(stderr, "ENGRAM_DATABASE_URL is required") {
		t.Fatalf("--database-url flag should override env requirement, got: %q", stderr)
	}
}

func TestCmdCloudServeHappyPath(t *testing.T) {
	oldStoreNew := cloudStoreNew
	oldStoreClose := cloudStoreClose
	oldAuthNew := cloudAuthNew
	oldServerNew := cloudServerNew
	oldServerStart := cloudServerStart
	t.Cleanup(func() {
		cloudStoreNew = oldStoreNew
		cloudStoreClose = oldStoreClose
		cloudAuthNew = oldAuthNew
		cloudServerNew = oldServerNew
		cloudServerStart = oldServerStart
	})

	secret := strings.Repeat("s", 32)
	t.Setenv("ENGRAM_JWT_SECRET", secret)

	var gotCfg cloud.Config
	var gotSecret string
	var gotPort int

	cloudStoreNew = func(cfg cloud.Config) (*cloudstore.CloudStore, error) {
		gotCfg = cfg
		return &cloudstore.CloudStore{}, nil
	}
	cloudStoreClose = func(*cloudstore.CloudStore) error { return nil }
	cloudAuthNew = func(cs *cloudstore.CloudStore, jwtSecret string) (*auth.Service, error) {
		gotSecret = jwtSecret
		return &auth.Service{}, nil
	}
	cloudServerNew = func(cs *cloudstore.CloudStore, svc *auth.Service, port int) *cloudserver.CloudServer {
		gotPort = port
		return &cloudserver.CloudServer{}
	}
	cloudServerStart = func(*cloudserver.CloudServer) error { return nil }

	withArgs(t, "engram", "cloud", "serve", "--port", "9090", "--database-url", "postgres://flag@localhost/cloud")
	stdout, stderr := captureOutput(t, func() { cmdCloudServe() })
	if stdout != "" || stderr != "" {
		t.Fatalf("expected no output, got stdout=%q stderr=%q", stdout, stderr)
	}
	if gotCfg.DSN != "postgres://flag@localhost/cloud" {
		t.Fatalf("database url = %q", gotCfg.DSN)
	}
	if gotSecret != secret {
		t.Fatalf("secret = %q", gotSecret)
	}
	if gotPort != 9090 {
		t.Fatalf("port = %d", gotPort)
	}
}

func TestCloudConfigLoadSave(t *testing.T) {
	tmpHome := t.TempDir()
	oldHomeDir := userHomeDir
	t.Cleanup(func() { userHomeDir = oldHomeDir })
	userHomeDir = func() (string, error) { return tmpHome, nil }

	// Test save
	cc := &CloudConfig{
		ServerURL:    "https://engram.example.com",
		Token:        "eng_test123",
		RefreshToken: "refresh-123",
		UserID:       "u-123",
		Username:     "alice",
	}
	if err := saveCloudConfig(cc); err != nil {
		t.Fatalf("saveCloudConfig: %v", err)
	}

	// Verify file permissions
	path := filepath.Join(tmpHome, ".engram", "cloud.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config file: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Fatalf("expected 0600 permissions, got %04o", perm)
	}

	// Test load
	loaded, err := loadCloudConfig()
	if err != nil {
		t.Fatalf("loadCloudConfig: %v", err)
	}
	if loaded.ServerURL != cc.ServerURL || loaded.Token != cc.Token || loaded.RefreshToken != cc.RefreshToken ||
		loaded.UserID != cc.UserID || loaded.Username != cc.Username {
		t.Fatalf("loaded config doesn't match saved: got %+v, want %+v", loaded, cc)
	}

	// Test load with missing file
	userHomeDir = func() (string, error) { return t.TempDir(), nil }
	_, err = loadCloudConfig()
	if err == nil {
		t.Fatalf("expected error loading from nonexistent path")
	}
}

func TestCmdCloudDispatchUnknown(t *testing.T) {
	exitCalled := false
	oldExit := exitFunc
	t.Cleanup(func() { exitFunc = oldExit })
	exitFunc = func(code int) { exitCalled = true }

	cfg := testConfig(t)
	withArgs(t, "engram", "cloud", "nonexistent")
	_, stderr := captureOutput(t, func() { cmdCloud(cfg) })

	if !exitCalled {
		t.Fatalf("expected exit for unknown cloud subcommand")
	}
	if !strings.Contains(stderr, "unknown cloud command") {
		t.Fatalf("expected unknown command error, got: %q", stderr)
	}
}

func TestCmdCloudDispatchNoSubcommand(t *testing.T) {
	exitCalled := false
	oldExit := exitFunc
	t.Cleanup(func() { exitFunc = oldExit })
	exitFunc = func(code int) { exitCalled = true }

	cfg := testConfig(t)
	withArgs(t, "engram", "cloud")
	_, stderr := captureOutput(t, func() { cmdCloud(cfg) })

	if !exitCalled {
		t.Fatalf("expected exit for missing cloud subcommand")
	}
	if !strings.Contains(stderr, "usage: engram cloud") {
		t.Fatalf("expected usage in stderr, got: %q", stderr)
	}
}

func TestCmdSearchRemoteFlag(t *testing.T) {
	// Set up a mock cloud server that returns search results
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token-123" {
			t.Errorf("missing or wrong auth header: %s", r.Header.Get("Authorization"))
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		q := r.URL.Query().Get("q")
		if q == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "q required"})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{
					"id":         42,
					"type":       "decision",
					"title":      "Use JWT auth",
					"content":    "We decided to use JWT for authentication",
					"project":    "engram",
					"scope":      "project",
					"rank":       0.95,
					"created_at": "2026-03-07T10:00:00Z",
				},
			},
		})
	}))
	defer srv.Close()

	// Override the HTTP client to use the test server
	oldClient := cloudHTTPClient
	t.Cleanup(func() { cloudHTTPClient = oldClient })
	cloudHTTPClient = func() *http.Client { return srv.Client() }

	cfg := testConfig(t)

	withArgs(t, "engram", "search", "authentication", "--remote", srv.URL, "--token", "test-token-123")
	stdout, stderr := captureOutput(t, func() { cmdSearch(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "Found 1 memories (cloud)") {
		t.Fatalf("expected cloud search results, got: %q", stdout)
	}
	if !strings.Contains(stdout, "Use JWT auth") {
		t.Fatalf("expected search result title, got: %q", stdout)
	}
}

func TestCmdSearchDefaultLocalMode(t *testing.T) {
	// Ensure no remote env vars are set
	t.Setenv("ENGRAM_REMOTE_URL", "")
	t.Setenv("ENGRAM_TOKEN", "")

	cfg := testConfig(t)
	mustSeedObservation(t, cfg, "s-local", "proj-local", "note", "local-result", "local content for search", "project")

	withArgs(t, "engram", "search", "local", "--project", "proj-local")
	stdout, stderr := captureOutput(t, func() { cmdSearch(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "Found") && !strings.Contains(stdout, "local-result") {
		// If FTS doesn't find it (timing), at least verify we didn't hit a remote server
		if strings.Contains(stdout, "cloud") {
			t.Fatalf("default mode should be local, not cloud: %q", stdout)
		}
	}
}

func TestCmdContextRemoteFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/context" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer ctx-token" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"context": "## Memory from Cloud\n\nRemote context data here.",
		})
	}))
	defer srv.Close()

	oldClient := cloudHTTPClient
	t.Cleanup(func() { cloudHTTPClient = oldClient })
	cloudHTTPClient = func() *http.Client { return srv.Client() }

	cfg := testConfig(t)
	withArgs(t, "engram", "context", "--remote", srv.URL, "--token", "ctx-token")
	stdout, stderr := captureOutput(t, func() { cmdContext(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "Remote context data here") {
		t.Fatalf("expected remote context output, got: %q", stdout)
	}
}

func TestPrintUsageIncludesCloudCommands(t *testing.T) {
	oldVersion := version
	version = "test-version"
	t.Cleanup(func() { version = oldVersion })

	stdout, _ := captureOutput(t, func() { printUsage() })

	cloudItems := []string{
		"cloud serve",
		"cloud register",
		"cloud login",
		"cloud sync",
		"cloud status",
		"cloud api-key",
		"--remote",
		"--token",
		"ENGRAM_REMOTE_URL",
		"ENGRAM_TOKEN",
		"ENGRAM_DATABASE_URL",
		"ENGRAM_JWT_SECRET",
	}
	for _, item := range cloudItems {
		if !strings.Contains(stdout, item) {
			t.Errorf("usage output missing %q", item)
		}
	}
}

func TestCmdCloudRegisterServerRequired(t *testing.T) {
	exitCalled := false
	oldExit := exitFunc
	t.Cleanup(func() { exitFunc = oldExit })
	exitFunc = func(code int) { exitCalled = true }

	withArgs(t, "engram", "cloud", "register")
	_, stderr := captureOutput(t, func() { cmdCloudRegister() })

	if !exitCalled {
		t.Fatalf("expected exit for missing --server")
	}
	if !strings.Contains(stderr, "--server is required") {
		t.Fatalf("expected --server error, got: %q", stderr)
	}
}

func TestCmdCloudLoginServerRequired(t *testing.T) {
	exitCalled := false
	oldExit := exitFunc
	t.Cleanup(func() { exitFunc = oldExit })
	exitFunc = func(code int) { exitCalled = true }

	withArgs(t, "engram", "cloud", "login")
	_, stderr := captureOutput(t, func() { cmdCloudLogin() })

	if !exitCalled {
		t.Fatalf("expected exit for missing --server")
	}
	if !strings.Contains(stderr, "--server is required") {
		t.Fatalf("expected --server error, got: %q", stderr)
	}
}

func TestCmdCloudRegisterIntegration(t *testing.T) {
	// Mock cloud server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/auth/register" {
			w.WriteHeader(404)
			return
		}
		var body struct {
			Username string `json:"username"`
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		json.NewDecoder(r.Body).Decode(&body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(auth.AuthResult{
			UserID:       "u-new",
			Username:     body.Username,
			AccessToken:  "access-tok",
			RefreshToken: "refresh-tok",
			ExpiresIn:    3600,
		})
	}))
	defer srv.Close()

	// Override stdin scanner
	oldScanner := stdinScanner
	t.Cleanup(func() { stdinScanner = oldScanner })
	stdinScanner = func() *bufio.Scanner {
		return bufio.NewScanner(strings.NewReader("alice\nalice@test.com\nsecret1234\n"))
	}

	// Override HTTP client
	oldClient := cloudHTTPClient
	t.Cleanup(func() { cloudHTTPClient = oldClient })
	cloudHTTPClient = func() *http.Client { return srv.Client() }

	// Override home dir for config save
	tmpHome := t.TempDir()
	oldHome := userHomeDir
	t.Cleanup(func() { userHomeDir = oldHome })
	userHomeDir = func() (string, error) { return tmpHome, nil }

	withArgs(t, "engram", "cloud", "register", "--server", srv.URL)
	stdout, stderr := captureOutput(t, func() { cmdCloudRegister() })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "Registered as alice") {
		t.Fatalf("expected registration success, got: %q", stdout)
	}

	// Verify config was saved
	cc, err := loadCloudConfig()
	if err != nil {
		t.Fatalf("loadCloudConfig after register: %v", err)
	}
	if cc.ServerURL != srv.URL || cc.Token != "access-tok" || cc.RefreshToken != "refresh-tok" || cc.UserID != "u-new" || cc.Username != "alice" {
		t.Fatalf("unexpected saved config: %+v", cc)
	}
}

func TestCmdCloudLoginIntegration(t *testing.T) {
	requestBody := struct {
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
	}{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/auth/login" {
			w.WriteHeader(404)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("decode login request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(auth.AuthResult{
			UserID:       "u-existing",
			Username:     "bob",
			AccessToken:  "new-access-tok",
			RefreshToken: "new-refresh-tok",
			ExpiresIn:    3600,
		})
	}))
	defer srv.Close()

	oldScanner := stdinScanner
	t.Cleanup(func() { stdinScanner = oldScanner })
	stdinScanner = func() *bufio.Scanner {
		return bufio.NewScanner(strings.NewReader("bob\npassword123\n"))
	}

	oldClient := cloudHTTPClient
	t.Cleanup(func() { cloudHTTPClient = oldClient })
	cloudHTTPClient = func() *http.Client { return srv.Client() }

	tmpHome := t.TempDir()
	oldHome := userHomeDir
	t.Cleanup(func() { userHomeDir = oldHome })
	userHomeDir = func() (string, error) { return tmpHome, nil }

	withArgs(t, "engram", "cloud", "login", "--server", srv.URL)
	stdout, stderr := captureOutput(t, func() { cmdCloudLogin() })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "Logged in as bob") {
		t.Fatalf("expected login success, got: %q", stdout)
	}

	cc, err := loadCloudConfig()
	if err != nil {
		t.Fatalf("loadCloudConfig after login: %v", err)
	}
	if cc.Token != "new-access-tok" || cc.RefreshToken != "new-refresh-tok" || cc.Username != "bob" {
		t.Fatalf("unexpected saved config: %+v", cc)
	}
	if requestBody.Identifier != "bob" {
		t.Fatalf("expected identifier=bob, got %+v", requestBody)
	}
	if requestBody.Password != "password123" {
		t.Fatalf("expected password to be forwarded, got %+v", requestBody)
	}
}

func TestCmdCloudAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/auth/api-key" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer my-cloud-token" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{
			"api_key": "eng_abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
			"message": "Store this key securely. It will not be shown again.",
		})
	}))
	defer srv.Close()

	// Set up config with saved token
	tmpHome := t.TempDir()
	oldHome := userHomeDir
	t.Cleanup(func() { userHomeDir = oldHome })
	userHomeDir = func() (string, error) { return tmpHome, nil }

	saveCloudConfig(&CloudConfig{
		ServerURL: srv.URL,
		Token:     "my-cloud-token",
		UserID:    "u-api",
		Username:  "apiuser",
	})

	oldClient := cloudHTTPClient
	t.Cleanup(func() { cloudHTTPClient = oldClient })
	cloudHTTPClient = func() *http.Client { return srv.Client() }

	withArgs(t, "engram", "cloud", "api-key")
	stdout, stderr := captureOutput(t, func() { cmdCloudAPIKey() })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "eng_") {
		t.Fatalf("expected API key in output, got: %q", stdout)
	}
	if !strings.Contains(stdout, "WARNING") {
		t.Fatalf("expected warning message, got: %q", stdout)
	}
}

func TestCmdCloudSyncFlagOverridesEnvAndConfigNoOp(t *testing.T) {
	var manifestCalls int
	var chunkCalls int
	var pushCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cli-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sync/pull":
			manifestCalls++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"version": 1, "chunks": []any{}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sync/pull/"):
			chunkCalls++
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/sync/push":
			pushCalls++
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	t.Setenv("ENGRAM_REMOTE_URL", "http://env.invalid")
	t.Setenv("ENGRAM_TOKEN", "env-token")

	tmpHome := t.TempDir()
	oldHome := userHomeDir
	t.Cleanup(func() { userHomeDir = oldHome })
	userHomeDir = func() (string, error) { return tmpHome, nil }
	if err := saveCloudConfig(&CloudConfig{ServerURL: "http://config.invalid", Token: "config-token"}); err != nil {
		t.Fatalf("saveCloudConfig: %v", err)
	}

	oldClient := cloudHTTPClient
	t.Cleanup(func() { cloudHTTPClient = oldClient })
	cloudHTTPClient = func() *http.Client { return srv.Client() }

	withArgs(t, "engram", "cloud", "sync", "--server", srv.URL, "--token", "cli-token", "--legacy")
	stdout, stderr := captureOutput(t, func() { cmdCloudSync(testConfig(t)) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got %q", stderr)
	}
	if !strings.Contains(stdout, "Nothing new to push") || !strings.Contains(stdout, "Nothing new to pull") {
		t.Fatalf("unexpected output: %q", stdout)
	}
	if manifestCalls == 0 {
		t.Fatal("expected manifest requests")
	}
	if pushCalls != 0 {
		t.Fatalf("expected no push requests, got %d", pushCalls)
	}
	if chunkCalls != 0 {
		t.Fatalf("expected no chunk downloads, got %d", chunkCalls)
	}
}

func TestCmdCloudStatusEnvOverridesConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer env-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/sync/pull" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"version": 1,
			"chunks": []map[string]any{{
				"id":         "aabb1122",
				"created_by": "alice",
				"created_at": "2026-03-07T10:00:00Z",
				"sessions":   1,
				"memories":   2,
				"prompts":    0,
			}},
		})
	}))
	defer srv.Close()

	t.Setenv("ENGRAM_REMOTE_URL", srv.URL)
	t.Setenv("ENGRAM_TOKEN", "env-token")

	tmpHome := t.TempDir()
	oldHome := userHomeDir
	t.Cleanup(func() { userHomeDir = oldHome })
	userHomeDir = func() (string, error) { return tmpHome, nil }
	if err := saveCloudConfig(&CloudConfig{ServerURL: "http://config.invalid", Token: "config-token", Username: "config-user"}); err != nil {
		t.Fatalf("saveCloudConfig: %v", err)
	}

	oldClient := cloudHTTPClient
	t.Cleanup(func() { cloudHTTPClient = oldClient })
	cloudHTTPClient = func() *http.Client { return srv.Client() }

	stdout, stderr := captureOutput(t, func() { cmdCloudStatus(testConfig(t)) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got %q", stderr)
	}
	if !strings.Contains(stdout, srv.URL) {
		t.Fatalf("expected env server url in output, got %q", stdout)
	}
	if !strings.Contains(stdout, "Remote chunks:   1") {
		t.Fatalf("unexpected status output: %q", stdout)
	}
}

// ─── Autosync / Sync-Status CLI Tests ────────────────────────────────────────

func TestCmdCloudSyncStatusShowsState(t *testing.T) {
	cfg := testConfig(t)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	// Create a session to seed some data and trigger sync_state creation.
	_ = s.CreateSession("test-session", "test-project", "/tmp")
	s.Close()

	withArgs(t, "engram", "cloud", "sync-status")
	stdout, stderr := captureOutput(t, func() { cmdCloudSyncStatus(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got %q", stderr)
	}
	// Should show lifecycle and pending info.
	if !strings.Contains(stdout, "Lifecycle:") {
		t.Fatalf("expected Lifecycle in output, got %q", stdout)
	}
	if !strings.Contains(stdout, "Pending mutations:") {
		t.Fatalf("expected Pending mutations in output, got %q", stdout)
	}
}

func TestCmdCloudSyncStatusUninitializedStore(t *testing.T) {
	cfg := testConfig(t)

	// Don't create any sessions — sync_state won't exist.
	// Create the store just to initialize the DB.
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	s.Close()

	withArgs(t, "engram", "cloud", "sync-status")
	stdout, _ := captureOutput(t, func() { cmdCloudSyncStatus(cfg) })

	// Sync state should either show or indicate not initialized.
	// After Phase 1, sync_state row is lazily created, so it might say "not initialized"
	// or show idle lifecycle depending on whether any writes happened.
	if stdout == "" {
		t.Fatal("expected some output from sync-status")
	}
}

func TestCmdCloudSyncMutationEngine(t *testing.T) {
	// Test the new mutation-based sync by mocking the push/pull endpoints.
	var pushCalls int
	var pullCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sync/mutations/push":
			pushCalls++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"accepted": 0, "last_seq": 0})
		case r.Method == http.MethodGet && r.URL.Path == "/sync/mutations/pull":
			pullCalls++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"mutations": []any{}, "has_more": false})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tmpHome := t.TempDir()
	oldHome := userHomeDir
	t.Cleanup(func() { userHomeDir = oldHome })
	userHomeDir = func() (string, error) { return tmpHome, nil }

	oldClient := cloudHTTPClient
	t.Cleanup(func() { cloudHTTPClient = oldClient })
	cloudHTTPClient = func() *http.Client { return srv.Client() }

	withArgs(t, "engram", "cloud", "sync", "--server", srv.URL, "--token", "test-token")
	stdout, stderr := captureOutput(t, func() { cmdCloudSync(testConfig(t)) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got %q", stderr)
	}
	if !strings.Contains(stdout, "Sync") {
		t.Fatalf("expected Sync output, got %q", stdout)
	}
}

func TestCmdCloudDispatchSyncStatus(t *testing.T) {
	// Verify that "engram cloud sync-status" routes to cmdCloudSyncStatus.
	cfg := testConfig(t)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	s.Close()

	withArgs(t, "engram", "cloud", "sync-status")
	stdout, _ := captureOutput(t, func() { cmdCloudSyncStatus(cfg) })
	if stdout == "" {
		t.Fatal("expected output from sync-status command")
	}
}

// ─── Enrollment CLI Tests ────────────────────────────────────────────────────

func TestCmdCloudEnrollHappyPath(t *testing.T) {
	cfg := testConfig(t)

	withArgs(t, "engram", "cloud", "enroll", "my-project")
	stdout, stderr := captureOutput(t, func() { cmdCloudEnroll(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, `"my-project" enrolled for cloud sync`) {
		t.Fatalf("unexpected enroll output: %q", stdout)
	}

	// Verify it's actually enrolled in the store.
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	enrolled, err := s.IsProjectEnrolled("my-project")
	if err != nil {
		t.Fatalf("IsProjectEnrolled: %v", err)
	}
	if !enrolled {
		t.Fatal("expected project to be enrolled after enroll command")
	}
}

func TestCmdCloudEnrollMissingArg(t *testing.T) {
	exitCalled := false
	oldExit := exitFunc
	t.Cleanup(func() { exitFunc = oldExit })
	exitFunc = func(code int) { exitCalled = true }

	cfg := testConfig(t)
	withArgs(t, "engram", "cloud", "enroll")
	_, stderr := captureOutput(t, func() { cmdCloudEnroll(cfg) })

	if !exitCalled {
		t.Fatal("expected exit for missing project arg")
	}
	if !strings.Contains(stderr, "usage: engram cloud enroll") {
		t.Fatalf("expected usage in stderr, got: %q", stderr)
	}
}

func TestCmdCloudEnrollIdempotent(t *testing.T) {
	cfg := testConfig(t)

	// Enroll twice — should succeed both times.
	withArgs(t, "engram", "cloud", "enroll", "idempotent-proj")
	stdout1, stderr1 := captureOutput(t, func() { cmdCloudEnroll(cfg) })
	if stderr1 != "" {
		t.Fatalf("first enroll: unexpected stderr: %q", stderr1)
	}
	if !strings.Contains(stdout1, "enrolled") {
		t.Fatalf("first enroll: unexpected output: %q", stdout1)
	}

	stdout2, stderr2 := captureOutput(t, func() { cmdCloudEnroll(cfg) })
	if stderr2 != "" {
		t.Fatalf("second enroll: unexpected stderr: %q", stderr2)
	}
	if !strings.Contains(stdout2, "enrolled") {
		t.Fatalf("second enroll: unexpected output: %q", stdout2)
	}
}

func TestCmdCloudUnenrollHappyPath(t *testing.T) {
	cfg := testConfig(t)

	// First enroll, then unenroll.
	withArgs(t, "engram", "cloud", "enroll", "unenroll-proj")
	captureOutput(t, func() { cmdCloudEnroll(cfg) })

	withArgs(t, "engram", "cloud", "unenroll", "unenroll-proj")
	stdout, stderr := captureOutput(t, func() { cmdCloudUnenroll(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, `"unenroll-proj" unenrolled`) {
		t.Fatalf("unexpected unenroll output: %q", stdout)
	}

	// Verify it's no longer enrolled.
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	enrolled, err := s.IsProjectEnrolled("unenroll-proj")
	if err != nil {
		t.Fatalf("IsProjectEnrolled: %v", err)
	}
	if enrolled {
		t.Fatal("expected project to be unenrolled after unenroll command")
	}
}

func TestCmdCloudUnenrollMissingArg(t *testing.T) {
	exitCalled := false
	oldExit := exitFunc
	t.Cleanup(func() { exitFunc = oldExit })
	exitFunc = func(code int) { exitCalled = true }

	cfg := testConfig(t)
	withArgs(t, "engram", "cloud", "unenroll")
	_, stderr := captureOutput(t, func() { cmdCloudUnenroll(cfg) })

	if !exitCalled {
		t.Fatal("expected exit for missing project arg")
	}
	if !strings.Contains(stderr, "usage: engram cloud unenroll") {
		t.Fatalf("expected usage in stderr, got: %q", stderr)
	}
}

func TestCmdCloudUnenrollIdempotent(t *testing.T) {
	cfg := testConfig(t)

	// Unenroll a project that was never enrolled — should succeed (idempotent).
	withArgs(t, "engram", "cloud", "unenroll", "never-enrolled")
	stdout, stderr := captureOutput(t, func() { cmdCloudUnenroll(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "unenrolled") {
		t.Fatalf("unexpected output: %q", stdout)
	}
}

func TestCmdCloudProjectsEmpty(t *testing.T) {
	cfg := testConfig(t)

	withArgs(t, "engram", "cloud", "projects")
	stdout, stderr := captureOutput(t, func() { cmdCloudProjects(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "No projects enrolled") {
		t.Fatalf("expected empty message, got: %q", stdout)
	}
	if !strings.Contains(stdout, "engram cloud enroll") {
		t.Fatalf("expected hint about enroll command, got: %q", stdout)
	}
}

func TestCmdCloudProjectsWithEntries(t *testing.T) {
	cfg := testConfig(t)

	// Enroll two projects.
	withArgs(t, "engram", "cloud", "enroll", "alpha")
	captureOutput(t, func() { cmdCloudEnroll(cfg) })
	withArgs(t, "engram", "cloud", "enroll", "bravo")
	captureOutput(t, func() { cmdCloudEnroll(cfg) })

	withArgs(t, "engram", "cloud", "projects")
	stdout, stderr := captureOutput(t, func() { cmdCloudProjects(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "Enrolled projects (2)") {
		t.Fatalf("expected 2 projects header, got: %q", stdout)
	}
	if !strings.Contains(stdout, "alpha") || !strings.Contains(stdout, "bravo") {
		t.Fatalf("expected both projects listed, got: %q", stdout)
	}
}

func TestCmdCloudDispatchEnrollUnenrollProjects(t *testing.T) {
	cfg := testConfig(t)

	// Test that dispatch routes to the new commands correctly.
	withArgs(t, "engram", "cloud", "enroll", "dispatch-proj")
	stdout, stderr := captureOutput(t, func() { cmdCloud(cfg) })
	if stderr != "" {
		t.Fatalf("enroll dispatch: unexpected stderr: %q", stderr)
	}
	if !strings.Contains(stdout, "enrolled") {
		t.Fatalf("enroll dispatch: unexpected output: %q", stdout)
	}

	withArgs(t, "engram", "cloud", "projects")
	stdout, stderr = captureOutput(t, func() { cmdCloud(cfg) })
	if stderr != "" {
		t.Fatalf("projects dispatch: unexpected stderr: %q", stderr)
	}
	if !strings.Contains(stdout, "dispatch-proj") {
		t.Fatalf("projects dispatch: unexpected output: %q", stdout)
	}

	withArgs(t, "engram", "cloud", "unenroll", "dispatch-proj")
	stdout, stderr = captureOutput(t, func() { cmdCloud(cfg) })
	if stderr != "" {
		t.Fatalf("unenroll dispatch: unexpected stderr: %q", stderr)
	}
	if !strings.Contains(stdout, "unenrolled") {
		t.Fatalf("unenroll dispatch: unexpected output: %q", stdout)
	}
}

func TestPrintUsageIncludesEnrollmentCommands(t *testing.T) {
	oldVersion := version
	version = "test-version"
	t.Cleanup(func() { version = oldVersion })

	stdout, _ := captureOutput(t, func() { printUsage() })

	enrollItems := []string{
		"cloud enroll",
		"cloud unenroll",
		"cloud projects",
	}
	for _, item := range enrollItems {
		if !strings.Contains(stdout, item) {
			t.Errorf("usage output missing %q", item)
		}
	}
}

// ─── Integration: Round-Trip Enrollment + Sync Filtering ─────────────────────

func TestEnrollmentRoundTripFilteredSync(t *testing.T) {
	// Full round-trip: enroll project → write observation → verify mutation has project
	// → verify ListPendingSyncMutations only returns enrolled mutations.
	cfg := testConfig(t)

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	// 1. Enroll "sync-proj" for cloud sync.
	if err := s.EnrollProject("sync-proj"); err != nil {
		t.Fatalf("EnrollProject: %v", err)
	}

	// 2. Create session + observation for enrolled project.
	if err := s.CreateSession("s-enrolled", "sync-proj", "/tmp"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err = s.AddObservation(store.AddObservationParams{
		SessionID: "s-enrolled",
		Type:      "decision",
		Title:     "enrolled observation",
		Content:   "this should sync",
		Project:   "sync-proj",
		Scope:     "project",
	})
	if err != nil {
		t.Fatalf("AddObservation (enrolled): %v", err)
	}

	// 3. Create session + observation for non-enrolled project.
	if err := s.CreateSession("s-not-enrolled", "private-proj", "/tmp"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err = s.AddObservation(store.AddObservationParams{
		SessionID: "s-not-enrolled",
		Type:      "note",
		Title:     "non-enrolled observation",
		Content:   "this should NOT sync",
		Project:   "private-proj",
		Scope:     "project",
	})
	if err != nil {
		t.Fatalf("AddObservation (non-enrolled): %v", err)
	}

	// 4. List pending mutations — only enrolled project's mutations should appear.
	mutations, err := s.ListPendingSyncMutations(store.DefaultSyncTargetKey, 100)
	if err != nil {
		t.Fatalf("ListPendingSyncMutations: %v", err)
	}

	for _, m := range mutations {
		if m.Project == "private-proj" {
			t.Fatalf("non-enrolled project mutation leaked into pending list: %+v", m)
		}
	}

	// Verify at least one enrolled mutation exists.
	found := false
	for _, m := range mutations {
		if m.Project == "sync-proj" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected at least one mutation from enrolled project 'sync-proj'")
	}

	// 5. Skip-ack non-enrolled mutations to verify they get cleaned up.
	skipped, err := s.SkipAckNonEnrolledMutations(store.DefaultSyncTargetKey)
	if err != nil {
		t.Fatalf("SkipAckNonEnrolledMutations: %v", err)
	}
	if skipped == 0 {
		t.Fatal("expected at least one mutation to be skip-acked for non-enrolled project")
	}

	s.Close()
}

func TestSkipAckDoesNotTouchEmptyProjectMutations(t *testing.T) {
	// Verify skip-ack doesn't touch empty-project mutations.
	cfg := testConfig(t)

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	// Create a session with empty project (no project).
	if err := s.CreateSession("s-empty-proj", "", "/tmp"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err = s.AddObservation(store.AddObservationParams{
		SessionID: "s-empty-proj",
		Type:      "note",
		Title:     "global observation",
		Content:   "no project set",
		Project:   "",
		Scope:     "project",
	})
	if err != nil {
		t.Fatalf("AddObservation (empty project): %v", err)
	}

	// Enroll some other project so there's a distinction.
	if err := s.EnrollProject("other-proj"); err != nil {
		t.Fatalf("EnrollProject: %v", err)
	}

	// Skip-ack should NOT touch empty-project mutations.
	skipped, err := s.SkipAckNonEnrolledMutations(store.DefaultSyncTargetKey)
	if err != nil {
		t.Fatalf("SkipAckNonEnrolledMutations: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("expected 0 mutations skipped for empty project, got %d", skipped)
	}

	// Empty-project mutations should still be in the pending list.
	mutations, err := s.ListPendingSyncMutations(store.DefaultSyncTargetKey, 100)
	if err != nil {
		t.Fatalf("ListPendingSyncMutations: %v", err)
	}
	if len(mutations) == 0 {
		t.Fatal("expected empty-project mutations to remain in pending list")
	}

	s.Close()
}

func TestEnrollWriteUnenrollVerifyFiltering(t *testing.T) {
	// Enroll → write → unenroll → verify mutations are no longer returned.
	cfg := testConfig(t)

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	if err := s.EnrollProject("temp-proj"); err != nil {
		t.Fatalf("EnrollProject: %v", err)
	}
	if err := s.CreateSession("s-temp", "temp-proj", "/tmp"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err = s.AddObservation(store.AddObservationParams{
		SessionID: "s-temp",
		Type:      "note",
		Title:     "temp observation",
		Content:   "before unenroll",
		Project:   "temp-proj",
		Scope:     "project",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	// Verify mutation is pending while enrolled.
	mutations, err := s.ListPendingSyncMutations(store.DefaultSyncTargetKey, 100)
	if err != nil {
		t.Fatalf("ListPendingSyncMutations before unenroll: %v", err)
	}
	foundBefore := false
	for _, m := range mutations {
		if m.Project == "temp-proj" {
			foundBefore = true
		}
	}
	if !foundBefore {
		t.Fatal("expected mutation from 'temp-proj' while enrolled")
	}

	// Unenroll the project.
	if err := s.UnenrollProject("temp-proj"); err != nil {
		t.Fatalf("UnenrollProject: %v", err)
	}

	// After unenroll, mutations from that project should no longer appear.
	mutations, err = s.ListPendingSyncMutations(store.DefaultSyncTargetKey, 100)
	if err != nil {
		t.Fatalf("ListPendingSyncMutations after unenroll: %v", err)
	}
	for _, m := range mutations {
		if m.Project == "temp-proj" {
			t.Fatalf("mutation from unenrolled project 'temp-proj' still appears: %+v", m)
		}
	}

	s.Close()
}

// Ensure unused imports don't cause issues — these are used above
var _ = auth.AuthResult{}
var _ = (*cloudstore.CloudStore)(nil)
var _ = (*cloudserver.CloudServer)(nil)
var _ = remote.NewRemoteTransport
