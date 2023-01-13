package kubo

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type VersionMap map[string]VersionInfo

type VersionInfo struct {
	URL string
	CID string
}

type versionJSON struct {
	Platforms map[string]platformJSON
}

type platformJSON struct {
	Archs map[string]archJSON
}

type archJSON struct {
	Link   string
	CID    string
	SHA512 string
}

func FetchVersions(ctx context.Context) (VersionMap, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wg := sync.WaitGroup{}
	versionChan := make(chan string)
	errChan := make(chan error)

	wg.Add(1)
	go func() {
		defer wg.Done()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://dist.ipfs.tech/kubo/versions", nil)
		if err != nil {
			errChan <- err
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errChan <- err
			return
		}
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			versionChan <- strings.TrimSpace(scanner.Text())
		}
		close(versionChan)
	}()

	m := sync.Mutex{}
	versionMap := VersionMap{}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for version := range versionChan {
				url := fmt.Sprintf("https://dist.ipfs.tech/kubo/%s/dist.json", version)
				req, err := http.NewRequest(http.MethodGet, url, nil)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					errChan <- err
					return
				}
				var v versionJSON
				err = json.NewDecoder(resp.Body).Decode(&v)
				resp.Body.Close()
				if err != nil {
					errChan <- err
					return
				}
				arch := v.Platforms["linux"].Archs["amd64"]
				m.Lock()
				versionMap[version] = VersionInfo{
					URL: fmt.Sprintf("https://dist.ipfs.tech/kubo/%s%s", version, arch.Link),
					CID: arch.CID,
				}
				m.Unlock()
			}
		}()
	}
	go func() {
		wg.Wait()
		close(errChan)
	}()
	for err := range errChan {
		return nil, err
	}
	wg.Wait()
	return versionMap, nil
}

func (m VersionMap) FetchArchiveWithCaching(ctx context.Context, version string) (io.ReadCloser, error) {
	vi, ok := m[version]
	if !ok {
		return nil, fmt.Errorf("no such version %q", version)
	}
	tempDir := os.TempDir()
	archivePath := filepath.Join(tempDir, vi.CID)

	_, err := os.Stat(archivePath)
	if err != nil {
		if os.IsNotExist(err) {
			rc, err := m.FetchArchive(ctx, version)
			if err != nil {
				return nil, fmt.Errorf("fetching: %w", err)
			}
			defer rc.Close()

			f, err := os.Create(archivePath)
			if err != nil {
				return nil, fmt.Errorf("creating archive: %w", err)
			}
			_, err = io.Copy(f, rc)
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("copying archive: %w", err)
			}
			f.Close()
		} else {
			return nil, fmt.Errorf("stat'ing %q: %w", archivePath, err)
		}
	}
	// TODO checksum
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}
	return f, err
}

func (m VersionMap) FetchArchive(ctx context.Context, version string) (io.ReadCloser, error) {
	vi, ok := m[version]
	if !ok {
		return nil, fmt.Errorf("no such version %q", version)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, vi.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("building req: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching kubo archive: %w", err)
	}
	return resp.Body, nil
}
