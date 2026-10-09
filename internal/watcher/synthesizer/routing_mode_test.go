package synthesizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestFileSynthesizer_Synthesize_RoutingMode(t *testing.T) {
	for _, tt := range []struct {
		name string
		mode any
		want string // empty means the attribute must be absent
	}{
		{name: "preserve", mode: "preserve", want: "preserve"},
		{name: "focus normalized", mode: " Focus ", want: "focus"},
		{name: "normal", mode: "normal"},
		{name: "unknown", mode: "bogus"},
		{name: "non string", mode: 3},
		{name: "absent", mode: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			authData := map[string]any{"type": "claude"}
			if tt.mode != nil {
				authData["routing_mode"] = tt.mode
			}
			data, _ := json.Marshal(authData)
			if errWrite := os.WriteFile(filepath.Join(tempDir, "auth.json"), data, 0o644); errWrite != nil {
				t.Fatalf("write auth file: %v", errWrite)
			}
			ctx := &SynthesisContext{
				Config:      &config.Config{},
				AuthDir:     tempDir,
				Now:         time.Now(),
				IDGenerator: NewStableIDGenerator(),
			}
			auths, errSynthesize := NewFileSynthesizer().Synthesize(ctx)
			if errSynthesize != nil || len(auths) != 1 {
				t.Fatalf("Synthesize() = %d auths, err=%v", len(auths), errSynthesize)
			}
			got, ok := auths[0].Attributes[coreauth.AttributeRoutingMode]
			if tt.want == "" {
				if ok {
					t.Fatalf("routing_mode attribute = %q, want absent", got)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("routing_mode attribute = %q, want %q", got, tt.want)
			}
		})
	}
}
