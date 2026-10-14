package dashboards

import "crypto/rand"
import "encoding/hex"

func randomID() string {
	b := make([]byte, 5)
	rand.Read(b)
	return hex.EncodeToString(b)
}
