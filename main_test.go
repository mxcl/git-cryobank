package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCleanRejectsChangesAndUntrackedFiles(t *testing.T) {
	repo := testRepo(t)
	if err := clean(repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := clean(repo); err == nil {
		t.Fatal("clean accepted an untracked file")
	}
	os.Remove(filepath.Join(repo, "new.txt"))
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := clean(repo); err == nil {
		t.Fatal("clean accepted a modified file")
	}
}

func TestRemoteResumeFinalizeAndBrowse(t *testing.T) {
	repo := testRepo(t)
	runGit(t, repo, "tag", "-a", "v1", "-m", "version one")
	runGit(t, repo, "branch", "old")
	runGit(t, repo, "update-ref", "refs/remotes/origin/legacy", "HEAD")
	runGit(t, repo, "switch", "--detach")
	if err := os.WriteFile(filepath.Join(repo, "detached.txt"), []byte("preserved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "detached.txt")
	runGit(t, repo, "commit", "-m", "detached head")
	head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	runGit(t, repo, "bundle", "create", bundle, "--all", "HEAD")
	digest, size, err := fileDigest(bundle)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	setArchiveRoot(t, root)
	data, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	half := int64(len(data) / 2)

	withStdin(t, data[:half], func() {
		err := remote([]string{"upload", "project", digest, strconv.FormatInt(size, 10), "0"})
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("partial upload error = %v", err)
		}
	})
	partial := filepath.Join(root, ".uploads", digest+".bundle")
	if info, err := os.Stat(partial); err != nil || info.Size() != half {
		t.Fatalf("partial upload size = %v, %v", info, err)
	}
	withStdin(t, data[half:], func() {
		if err := remote([]string{"upload", "project", digest, strconv.FormatInt(size, 10), strconv.FormatInt(half, 10)}); err != nil {
			t.Fatal(err)
		}
	})
	if err := remote([]string{"commit", "project", digest, strconv.FormatInt(size, 10)}); err != nil {
		t.Fatal(err)
	}
	if err := remote([]string{"commit", "project", digest, strconv.FormatInt(size, 10)}); err != nil {
		t.Fatalf("idempotent commit: %v", err)
	}
	archivedRepo := filepath.Join(root, "project.git")
	if got := strings.TrimSpace(runGit(t, archivedRepo, "rev-parse", "HEAD")); got != head {
		t.Fatalf("archived HEAD = %s, want %s", got, head)
	}
	refs := runGit(t, archivedRepo, "show-ref")
	for _, ref := range []string{"refs/heads/main", "refs/heads/old", "refs/tags/v1"} {
		if !strings.Contains(refs, ref) {
			t.Errorf("archive missing %s", ref)
		}
	}
	runGit(t, archivedRepo, "update-ref", "refs/heads/after-archive", "HEAD")
	if archived(root, "project", digest) {
		t.Fatal("mutated remote repository was accepted as the original archive")
	}

	ts := httptest.NewServer(webHandler(root))
	defer ts.Close()
	for _, path := range []string{"/", "/project/", "/project/tree?ref=HEAD", "/project/blob?ref=HEAD&path=README.md"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %s: %s", path, resp.Status, body)
		}
		if path == "/project/" {
			for _, branch := range []string{">main</a>", ">old</a>", ">remotes/origin/legacy</a>"} {
				if !bytes.Contains(body, []byte(branch)) {
					t.Errorf("repository page missing branch %q", branch)
				}
			}
		}
	}
}

func TestRejectsUnsafeArchiveName(t *testing.T) {
	setArchiveRoot(t, t.TempDir())
	if err := remote([]string{"probe", "../escape", strings.Repeat("a", 64), "10"}); err == nil {
		t.Fatal("unsafe archive name accepted")
	}
}

func TestArchiveRootUsesConfig(t *testing.T) {
	want := t.TempDir()
	setArchiveRoot(t, want)
	got, err := archiveRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("archiveRoot() = %q, want configured value %q", got, want)
	}
}

func TestArchiveRootRequiresConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := archiveRoot(); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("archiveRoot() error = %v, want configuration error", err)
	}
}

func TestRefStateChangesWithHEADAndRefs(t *testing.T) {
	repo := testRepo(t)
	before, err := refState(repo)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "branch", "another")
	after, err := refState(repo)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("ref fingerprint did not change after adding a branch")
	}
}

func testRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	return repo
}

func setArchiveRoot(t *testing.T, root string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "git-cryobank")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root"), []byte(root+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func withStdin(t *testing.T, data []byte, fn func()) {
	t.Helper()
	original := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	w.Close()
	os.Stdin = r
	defer func() { os.Stdin = original; r.Close() }()
	fn()
}
