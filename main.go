package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const usage = `git attic HOST [PATH]
git-attic init ROOT
git-attic serve [--root DIR] [--listen ADDR]

Archives a clean repository over SSH, verifies it remotely, then moves the
local repository to the macOS Trash. PATH defaults to the current directory.`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "git-attic:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "init":
		return initialize(args[1:])
	case "serve":
		return serve(args[1:])
	case "probe", "upload", "commit":
		return remote(args)
	case "-h", "--help", "help":
		fmt.Println(usage)
		return nil
	default:
		return archive(args)
	}
}

func archive(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: git attic HOST [PATH]")
	}
	host := args[0]
	path := "."
	if len(args) == 2 {
		path = args[1]
	}
	repo, err := repository(path)
	if err != nil {
		return err
	}
	if err := clean(repo); err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "git-attic-*.bundle")
	if err != nil {
		return err
	}
	bundle := tmp.Name()
	tmp.Close()
	defer os.Remove(bundle)
	if err := command(repo, "git", "bundle", "create", bundle, "--all", "HEAD"); err != nil {
		return err
	}
	digest, size, err := fileDigest(bundle)
	if err != nil {
		return err
	}
	name := filepath.Base(repo)
	if !safeName.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("repository directory name %q is unsafe; use only letters, digits, dot, underscore, and hyphen", name)
	}

	offsetText, err := ssh(host, nil, "probe", name, digest, strconv.FormatInt(size, 10))
	if err != nil {
		return err
	}
	offset, err := strconv.ParseInt(strings.TrimSpace(offsetText), 10, 64)
	if err != nil || offset < 0 || offset > size {
		return fmt.Errorf("invalid upload offset from server: %q", offsetText)
	}
	if offset < size {
		f, err := os.Open(bundle)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := ssh(host, f, "upload", name, digest, strconv.FormatInt(size, 10), strconv.FormatInt(offset, 10)); err != nil {
			return err
		}
	}
	confirmation, err := ssh(host, nil, "commit", name, digest, strconv.FormatInt(size, 10))
	if err != nil {
		return err
	}
	if strings.TrimSpace(confirmation) != "archived "+digest {
		return fmt.Errorf("remote verification failed: %s", strings.TrimSpace(confirmation))
	}
	if runtime.GOOS != "darwin" {
		return errors.New("remote archive verified, but refusing removal: Trash is only supported on macOS")
	}
	if err := command("", "/usr/bin/trash", repo); err != nil {
		return fmt.Errorf("remote archive verified, but moving local repository to Trash failed: %w", err)
	}
	fmt.Printf("Archived %s to %s as %s and moved it to Trash.\n", repo, host, name)
	return nil
}

func repository(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	out, err := output(abs, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", errors.New("path is not inside a Git working tree")
	}
	repo := strings.TrimSpace(out)
	if filepath.Clean(abs) != filepath.Clean(repo) {
		info, statErr := os.Stat(abs)
		if statErr != nil || !info.IsDir() {
			return "", errors.New("PATH must name the repository root")
		}
	}
	return repo, nil
}

func clean(repo string) error {
	out, err := output(repo, "git", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if out != "" {
		return errors.New("repository has staged, modified, or untracked files; nothing was archived")
	}
	if _, err := output(repo, "git", "rev-parse", "--verify", "HEAD"); err != nil {
		return errors.New("repository has no HEAD commit")
	}
	return nil
}

func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func ssh(host string, stdin io.Reader, args ...string) (string, error) {
	if host == "" || strings.HasPrefix(host, "-") {
		return "", errors.New("invalid SSH host")
	}
	remoteCommand := "git-attic " + strings.Join(args, " ")
	cmd := exec.Command("ssh", "--", host, remoteCommand)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh %s: %w: %s", host, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func command(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

func output(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	b, err := cmd.Output()
	return string(b), err
}
