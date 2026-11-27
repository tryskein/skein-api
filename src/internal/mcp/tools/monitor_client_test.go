package tools

import "testing"

// TestValidWindowsTokenDir — US-11, R-7: a tokenDir is an absolute path with
// a drive letter whose components Windows takes as written. A component
// ending in a dot or a space is refused: Windows path resolution strips
// trailing dots and spaces, so ".. " and "..." step to the parent and "x."
// names another directory than the server builds in.
func TestValidWindowsTokenDir(t *testing.T) {
	accepted := []string{`C:\Users\me\AppData\Local`, `D:/data/agent dir`, `C:\Users\O'Brien\.config`, `C:\`}
	refused := []string{`C:\Users\..\Public`, `C:\Users\.. \Public`, `C:\Users\...\Public`, `C:\Users\. \me`,
		`C:\Users\me\.`, `C:\Users\dir.\me`, `C:\Users\dir \me`, `C:/Users/../Public`}
	for _, d := range accepted {
		if !validWindowsTokenDir(d) {
			t.Fatalf("%q was refused", d)
		}
	}
	for _, d := range refused {
		if validWindowsTokenDir(d) {
			t.Fatalf("%q was accepted", d)
		}
	}
}
