//go:build knownbugs

package authz

import "testing"

// 此規格在階段 3 修復：同群組內先採最長前綴，再與其他群組取最寬鬆。
func TestKnownBugLongestPrefixWinsWithinGroup(t *testing.T) {
	a, err := Load(writePerms(t, `{"default":"none","groups":{
	  "editors":{"members":["local:alice"],"rules":[
	    {"path":"","access":"write"},
	    {"path":"private","access":"read"}
	  ]}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.Can("local:alice", "private/note.md", AccessWrite) {
		t.Fatal("較窄的 private:read 應覆蓋同群組的根 write")
	}
	if !a.Can("local:alice", "private/note.md", AccessRead) {
		t.Fatal("private 應保留 read")
	}
}
