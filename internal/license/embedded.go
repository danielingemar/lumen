package license

import "embed"

//go:embed keys
var keyFS embed.FS

// EmbeddedKeys are the keys built into this program (the .pub files of the keys directory).
func EmbeddedKeys() Keys {
	k, err := LoadKeys(keyFS, "keys")
	if err != nil {
		panic("licence keys built into the program are damaged: " + err.Error())
	}
	return k
}
