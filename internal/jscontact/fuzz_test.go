package jscontact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzFromVCard: any parseable vCard converts without panicking, into
// something that marshals as JSON.
func FuzzFromVCard(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.vcf")
	for _, p := range files {
		b, _ := os.ReadFile(p)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		card, err := Codec{}.ToJMAP(raw)
		if err != nil {
			return
		}
		if _, err := json.Marshal(card); err != nil {
			t.Fatal(err)
		}
	})
}
