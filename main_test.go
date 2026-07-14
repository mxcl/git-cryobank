package main

import (
	"bytes"
	"fmt"
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

func TestMain(m *testing.M) {
	if os.Getenv("CRYOBANK_SHELL_HELPER") == "1" {
		if err := shell(nil); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
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
	setConfig(t, "root", root)
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
	card, err := repositoryCard(root, "project")
	if err != nil || card.Status != "frozen" {
		t.Fatalf("repository card = %#v, %v", card, err)
	}
	runGit(t, archivedRepo, "config", "cryobank.frozenAt", "2020-01-01T00:00:00Z")
	card, err = repositoryCard(root, "project")
	if err != nil || card.Status != "deep-archive" {
		t.Fatalf("old repository card = %#v, %v", card, err)
	}
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
	setConfig(t, "root", t.TempDir())
	if err := remote([]string{"probe", "../escape", strings.Repeat("a", 64), "10"}); err == nil {
		t.Fatal("unsafe archive name accepted")
	}
}

func TestArchiveRootUsesConfig(t *testing.T) {
	want := t.TempDir()
	setConfig(t, "root", want)
	got, err := archiveRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("archiveRoot() = %q, want configured value %q", got, want)
	}
}

func TestArchiveTargetUsesGitConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runGit(t, "", "config", "--global", "cryobank.host", "pangolin")
	got, err := archiveTarget()
	if err != nil {
		t.Fatal(err)
	}
	if got != "pangolin" {
		t.Fatalf("archiveTarget() = %q, want pangolin", got)
	}
	runGit(t, "", "config", "--global", "cryobank.host", "-unsafe")
	if _, err := archiveTarget(); err == nil {
		t.Fatal("archiveTarget accepted an unsafe host")
	}
}

func TestArchiveTargetRequiresConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := archiveTarget(); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("archiveTarget() error = %v, want configuration error", err)
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

func TestFreezeSnapshotCapturesDirtyAndUntrackedFiles(t *testing.T) {
	repo := testRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state, restore, err := freezeSnapshot(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if got := strings.TrimSpace(runGit(t, repo, "show", "refs/cryobank/freeze:README.md")); got != "changed" {
		t.Fatalf("snapshot README = %q", got)
	}
	if got := strings.TrimSpace(runGit(t, repo, "show", "refs/cryobank/freeze:new.txt")); got != "new" {
		t.Fatalf("snapshot new.txt = %q", got)
	}
	if state.Branch != "main" || state.Base == "" || state.Tree == "" {
		t.Fatalf("snapshot state = %#v", state)
	}
}

func TestRestoreSnapshotRecreatesDirtyCheckout(t *testing.T) {
	source := testRepo(t)
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "untracked.txt"), []byte("keep me\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, restoreRef, err := freezeSnapshot(source, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restoreRef()
	bare := filepath.Join(t.TempDir(), "project.git")
	runGit(t, "", "clone", "--mirror", source, bare)
	dest := filepath.Join(t.TempDir(), "project")
	runGit(t, "", "clone", bare, dest)
	runGit(t, dest, "fetch", "origin", "+refs/cryobank/freeze:refs/cryobank/freeze")
	if err := restoreSnapshot(dest); err != nil {
		t.Fatal(err)
	}
	status := runGit(t, dest, "status", "--porcelain=v1")
	for _, want := range []string{" M README.md", "?? untracked.txt"} {
		if !strings.Contains(status, want) {
			t.Errorf("restored status missing %q:\n%s", want, status)
		}
	}
}

func TestShellPushAndClone(t *testing.T) {
	root := t.TempDir()
	setConfig(t, "root", root)
	wrapper := filepath.Join(t.TempDir(), "cryobank-shell")
	script := "#!/bin/sh\nexport CRYOBANK_SHELL_HELPER=1\nexport SSH_ORIGINAL_COMMAND=\"$GIT_EXT_SERVICE '$1'\"\nexec \"$CRYOBANK_TEST_BINARY\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRYOBANK_TEST_BINARY", os.Args[0])
	repo := testRepo(t)
	remote := "ext::" + wrapper + " project.git"
	runGit(t, repo, "remote", "add", "cryobank", remote)
	runGit(t, repo, "-c", "protocol.ext.allow=always", "push", "cryobank", "main")
	bare := filepath.Join(root, "project.git")
	if got := strings.TrimSpace(runGit(t, bare, "rev-parse", "HEAD")); got == "" {
		t.Fatal("pushed repository has no HEAD")
	}
	dest := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "-c", "protocol.ext.allow=always", "clone", remote, dest)
	if got := strings.TrimSpace(runGit(t, dest, "show", "HEAD:README.md")); got != "hello" {
		t.Fatalf("cloned README = %q", got)
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

func setConfig(t *testing.T, name, value string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "cryobank")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0600); err != nil {
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
