package gateway

import (
	"io"
	"strings"
)

func stringsTrim(value string) string {
	return strings.TrimSpace(value)
}

func stringsNewReader(value string) io.Reader {
	return strings.NewReader(value)
}
