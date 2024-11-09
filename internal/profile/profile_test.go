package profile

import (
	"path"
	"testing"

	"os"

	"github.com/stretchr/testify/assert"
)

func TestRemoveProfilePath(t *testing.T) {
	dir := t.TempDir()
	file1 := path.Join(dir, "file1")
	os.Create(file1)
	file2 := path.Join(dir, "file2")
	os.Create(file2)

	RemoveProfilePath(file1)
	entries, _ := os.ReadDir(dir)
	files := make([]string, len(entries))
	for i, e := range entries {
		files[i] = e.Name()
	}
	expected := []string{"file2"}
	assert.Equal(t, expected, files)
}