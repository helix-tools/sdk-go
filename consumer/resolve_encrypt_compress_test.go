package consumer

import (
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// TestResolveEncryptCompress pins the decrypt/decompress decision, including the
// create-endpoint promotion drift the SDK-only E2E suite surfaced 2026-07-06:
// metadata.encryption_enabled is promoted to the top-level `encryption` field
// and dropped from metadata, so a metadata-only read wrongly skipped decrypt.
func TestResolveEncryptCompress(t *testing.T) {
	cases := []struct {
		name     string
		dataset  *types.Dataset
		wantEnc  bool
		wantComp bool
	}{
		{
			name: "promotion drift: metadata.encryption_enabled absent, top-level encryption=true",
			// This is the exact shape the deployed create endpoint returns.
			dataset: &types.Dataset{
				Encryption: true,
				Metadata:   map[string]any{"compression_enabled": true},
			},
			wantEnc:  true, // MUST fall back to the top-level field
			wantComp: true,
		},
		{
			name: "metadata flags present override the top-level field",
			dataset: &types.Dataset{
				Encryption: false,
				Metadata:   map[string]any{"encryption_enabled": true, "compression_enabled": true},
			},
			wantEnc:  true,
			wantComp: true,
		},
		{
			name: "metadata says not encrypted, wins over top-level true",
			dataset: &types.Dataset{
				Encryption: true,
				Metadata:   map[string]any{"encryption_enabled": false, "compression_enabled": false},
			},
			wantEnc:  false,
			wantComp: false,
		},
		{
			name:     "no metadata, no top-level flag -> both false",
			dataset:  &types.Dataset{Encryption: false, Metadata: nil},
			wantEnc:  false,
			wantComp: false,
		},
		{
			name:     "no metadata, top-level encryption=true -> encrypted, not compressed",
			dataset:  &types.Dataset{Encryption: true, Metadata: nil},
			wantEnc:  true,
			wantComp: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotEnc, gotComp := resolveEncryptCompress(tc.dataset)
			if gotEnc != tc.wantEnc {
				t.Errorf("isEncrypted = %v, want %v", gotEnc, tc.wantEnc)
			}
			if gotComp != tc.wantComp {
				t.Errorf("isCompressed = %v, want %v", gotComp, tc.wantComp)
			}
		})
	}
}
