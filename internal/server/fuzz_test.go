package server

import "testing"

func FuzzParseSyncToken(f *testing.F) {
	const id = "f4b8d5074e087890"
	for _, s := range []string{"", formatSyncToken(id, 0), formatSyncToken(id, 42) + ".3", oldSyncTokenPrefix + id + "-7", "http://ccdav/ns/sync/x-", "garbage"} {
		f.Add(s, uint64(5))
	}
	f.Fuzz(func(t *testing.T, tok string, n uint64) {
		parseSyncToken(tok, id)
		if got, err := parseSyncToken(formatSyncToken(id, n), id); err != nil || got != n {
			t.Fatalf("round trip %d: %d, %v", n, got, err)
		}
	})
}
