package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPatchAuthFileFields_RoutingModeValidationPersistAndExposure(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "routing.json"
	filePath := filepath.Join(authDir, fileName)
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:         fileName,
		FileName:   fileName,
		Provider:   "codex",
		Attributes: map[string]string{"path": filePath},
		Metadata:   map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	patch := func(value string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		body := `{"name":"routing.json","routing_mode":` + value + `}`
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v8/management/credentials/fields", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PatchAuthFileFields(ctx)
		return rec
	}
	persistedMode := func() (any, bool) {
		t.Helper()
		raw, errRead := os.ReadFile(filePath)
		if errRead != nil {
			t.Fatalf("ReadFile() error = %v", errRead)
		}
		var persisted map[string]any
		if errUnmarshal := json.Unmarshal(raw, &persisted); errUnmarshal != nil {
			t.Fatalf("Unmarshal() error = %v", errUnmarshal)
		}
		value, exists := persisted["routing_mode"]
		return value, exists
	}
	exposed := func() any {
		t.Helper()
		updated, ok := manager.GetByID(fileName)
		if !ok {
			t.Fatal("auth missing")
		}
		return h.buildAuthFileEntry(updated)["routing_mode"]
	}

	for _, invalid := range []string{`"bogus"`, `5`, `true`, `{"a":1}`} {
		if rec := patch(invalid); rec.Code != http.StatusBadRequest {
			t.Fatalf("patch %s status = %d, want 400; body=%s", invalid, rec.Code, rec.Body.String())
		}
	}
	if got := exposed(); got != "normal" {
		t.Fatalf("exposed routing_mode after rejected patches = %v, want normal", got)
	}

	if rec := patch(`" Preserve "`); rec.Code != http.StatusOK {
		t.Fatalf("preserve status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	updated, _ := manager.GetByID(fileName)
	if updated.Attributes[coreauth.AttributeRoutingMode] != "preserve" || coreauth.RoutingMode(updated) != coreauth.RoutingModePreserve {
		t.Fatalf("runtime attributes = %v, want routing_mode=preserve", updated.Attributes)
	}
	if got := exposed(); got != "preserve" {
		t.Fatalf("exposed routing_mode = %v, want preserve", got)
	}
	if value, exists := persistedMode(); !exists || value != "preserve" {
		t.Fatalf("persisted routing_mode = %v (exists=%v), want preserve", value, exists)
	}

	if rec := patch(`"focus"`); rec.Code != http.StatusOK {
		t.Fatalf("focus status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := exposed(); got != "focus" {
		t.Fatalf("exposed routing_mode = %v, want focus", got)
	}

	for _, clear := range []string{`"normal"`, `null`} {
		if rec := patch(`"focus"`); rec.Code != http.StatusOK {
			t.Fatalf("re-focus status = %d; body=%s", rec.Code, rec.Body.String())
		}
		if rec := patch(clear); rec.Code != http.StatusOK {
			t.Fatalf("clear %s status = %d, want 200; body=%s", clear, rec.Code, rec.Body.String())
		}
		updated, _ = manager.GetByID(fileName)
		if _, exists := updated.Attributes[coreauth.AttributeRoutingMode]; exists {
			t.Fatalf("runtime routing_mode remains after %s: %v", clear, updated.Attributes)
		}
		if got := exposed(); got != "normal" {
			t.Fatalf("exposed routing_mode after %s = %v, want normal", clear, got)
		}
		if _, exists := persistedMode(); exists {
			t.Fatalf("persisted routing_mode remains after %s", clear)
		}
	}
}

func TestPatchAuthFileFields_RoutingModeRejectsNestedField(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	record := &coreauth.Auth{ID: "nested.json", FileName: "nested.json", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v8/management/credentials/fields", strings.NewReader(`{"name":"nested.json","routing_mode.x":"focus"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchAuthFileFields(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("nested status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBuildAuthFileEntry_ExposesRoutingModeFromAttributesOrMetadata(t *testing.T) {
	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(&memoryAuthStore{}, nil, nil))
	pathFor := func(name string) string {
		path := filepath.Join(authDir, name)
		if errWrite := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
			t.Fatalf("write %s: %v", name, errWrite)
		}
		return path
	}
	fromAttr := &coreauth.Auth{ID: "a.json", FileName: "a.json", Provider: "codex", Attributes: map[string]string{"path": pathFor("a.json"), coreauth.AttributeRoutingMode: "focus"}}
	fromMeta := &coreauth.Auth{ID: "b.json", FileName: "b.json", Provider: "codex", Attributes: map[string]string{"path": pathFor("b.json")}, Metadata: map[string]any{"routing_mode": "Preserve"}}
	plain := &coreauth.Auth{ID: "c.json", FileName: "c.json", Provider: "codex", Attributes: map[string]string{"path": pathFor("c.json")}}
	for auth, want := range map[*coreauth.Auth]string{fromAttr: "focus", fromMeta: "preserve", plain: "normal"} {
		if got := h.buildAuthFileEntry(auth)["routing_mode"]; got != want {
			t.Fatalf("%s routing_mode = %v, want %s", auth.ID, got, want)
		}
	}
}
