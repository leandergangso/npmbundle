package assets

import "embed"

//go:embed *.md *.py *.sh
var FS embed.FS

func MustReadFile(name string) []byte {
	b, err := FS.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return b
}
