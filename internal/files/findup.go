package files

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FindUp searches up the filesystem hierarchy starting at dir, for a file with the given name, returning an empty string if none is found.
func FindUp(name, dir string) string {
	curDir := dir
	for {
		entries, err := os.ReadDir(curDir)
		if err != nil {
			panic(err)
		}
		for _, e := range entries {
			if name == e.Name() {
				return filepath.Join(curDir, name)
			}
		}
		newDir := filepath.Dir(curDir)
		if newDir == curDir {
			return ""
		}
		curDir = newDir
	}
}

func FindNodeAgentBin() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting wd: %w", err)
	}
	nodeAgentBin := FindUp("nodeagent", wd)
	if nodeAgentBin == "" {
		return "", errors.New("unable to find nodeagent bin")
	}
	return nodeAgentBin, nil
}
