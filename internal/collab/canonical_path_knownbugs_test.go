//go:build knownbugs

package collab

import "testing"

// 此規格在階段 2 修復：等價路徑必須共用同一個 canonical room key。
func TestKnownBugEquivalentPathsShareRoom(t *testing.T) {
	h := newHub()
	h.addClient("notes/a.md", newClient(true))
	h.addClient("notes/./a.md", newClient(true))
	if len(h.rooms) != 1 {
		t.Fatalf("等價路徑形成了 %d 個房間，預期 1", len(h.rooms))
	}
}
