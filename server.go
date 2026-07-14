package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	safeName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	safeDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
	safeOID    = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	safeRef    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

func archiveRoot() (string, error) {
	root, err := readConfig("root")
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("cryobank is not configured; run: cryobank init ROOT")
	}
	if err != nil {
		return "", err
	}
	if root == "" {
		return "", errors.New("configured archive root is empty")
	}
	return filepath.Abs(root)
}

func initialize(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: cryobank init ROOT")
	}
	root, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if err := writeConfig("root", root); err != nil {
		return err
	}
	fmt.Println("Cryobank root configured at", root)
	return nil
}

func remote(args []string) error {
	if len(args) == 2 && args[0] == "activate" {
		name := args[1]
		if !safeName.MatchString(name) || name == "." || name == ".." {
			return errors.New("invalid repository name")
		}
		root, err := archiveRoot()
		if err != nil {
			return err
		}
		repo := filepath.Join(root, name+".git")
		if info, err := os.Stat(repo); err != nil || !info.IsDir() {
			return errors.New("repository does not exist")
		}
		_ = exec.Command("git", "-C", repo, "config", "--unset-all", "cryobank.frozenAt").Run()
		return exec.Command("git", "-C", repo, "config", "cryobank.activeAt", time.Now().UTC().Format(time.RFC3339)).Run()
	}
	if len(args) != 4 && len(args) != 5 {
		return errors.New("invalid receiver command")
	}
	op, name, digest, sizeText := args[0], args[1], args[2], args[3]
	if !safeName.MatchString(name) || name == "." || name == ".." || !safeDigest.MatchString(digest) {
		return errors.New("invalid archive name or digest")
	}
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || size <= 0 {
		return errors.New("invalid archive size")
	}
	root, err := archiveRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, ".uploads"), 0700); err != nil {
		return err
	}
	partial := filepath.Join(root, ".uploads", digest+".bundle")

	switch op {
	case "probe":
		if len(args) != 4 {
			return errors.New("invalid probe command")
		}
		if archived(root, name, digest) {
			fmt.Println(size)
			return nil
		}
		if _, err := os.Stat(filepath.Join(root, name+".git")); err == nil {
			return fmt.Errorf("archive name %q already contains different content", name)
		}
		info, err := os.Stat(partial)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println(0)
			return nil
		}
		if err != nil {
			return err
		}
		if info.Size() > size {
			if err := os.Remove(partial); err != nil {
				return err
			}
			fmt.Println(0)
			return nil
		}
		fmt.Println(info.Size())
		return nil

	case "upload":
		if len(args) != 5 {
			return errors.New("invalid upload command")
		}
		offset, err := strconv.ParseInt(args[4], 10, 64)
		if err != nil || offset < 0 || offset >= size {
			return errors.New("invalid upload offset")
		}
		f, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || info.Size() != offset {
			return errors.New("upload offset changed; retry the archive command")
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(f, os.Stdin, size-offset); err != nil {
			return fmt.Errorf("upload interrupted after partial data was saved: %w", err)
		}
		var extra [1]byte
		if n, _ := os.Stdin.Read(extra[:]); n != 0 {
			return errors.New("received more data than declared")
		}
		if err := f.Sync(); err != nil {
			return err
		}
		fmt.Println("uploaded")
		return nil

	case "commit":
		if len(args) != 4 {
			return errors.New("invalid commit command")
		}
		if archived(root, name, digest) {
			fmt.Println("archived " + digest)
			return nil
		}
		if err := finalize(root, name, digest, size, partial); err != nil {
			return err
		}
		fmt.Println("archived " + digest)
		return nil
	default:
		return errors.New("unknown receiver command")
	}
}

func archived(root, name, digest string) bool {
	repo := filepath.Join(root, name+".git")
	out, err := exec.Command("git", "-C", repo, "config", "--get", "cryobank.bundleSHA256").Output()
	if err != nil || strings.TrimSpace(string(out)) != digest {
		return false
	}
	want, err := exec.Command("git", "-C", repo, "config", "--get", "cryobank.refStateSHA256").Output()
	if err != nil {
		return false
	}
	got, err := refState(repo)
	return err == nil && strings.TrimSpace(string(want)) == got
}

func finalize(root, name, digest string, size int64, bundle string) error {
	info, err := os.Stat(bundle)
	if err != nil || info.Size() != size {
		return errors.New("upload is incomplete; retry the archive command")
	}
	f, err := os.Open(bundle)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	f.Close()
	if copyErr != nil || hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("uploaded bundle checksum mismatch")
	}

	dest := filepath.Join(root, name+".git")
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("archive name %q already exists", name)
	}
	tmp, err := os.MkdirTemp(root, ".incoming-")
	if err != nil {
		return err
	}
	os.Remove(tmp)
	defer os.RemoveAll(tmp)
	cmd := exec.Command("git", "clone", "--mirror", "--quiet", bundle, tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bundle verification failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := compareRefs(bundle, tmp); err != nil {
		return err
	}
	state, err := refState(tmp)
	if err != nil {
		return err
	}
	if err := exec.Command("git", "-C", tmp, "config", "cryobank.bundleSHA256", digest).Run(); err != nil {
		return err
	}
	if err := exec.Command("git", "-C", tmp, "config", "cryobank.refStateSHA256", state).Run(); err != nil {
		return err
	}
	if err := exec.Command("git", "-C", tmp, "config", "cryobank.frozenAt", time.Now().UTC().Format(time.RFC3339)).Run(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	return os.Remove(bundle)
}

var gitCommand = regexp.MustCompile(`^(git-upload-pack|git-receive-pack) ['"]?([A-Za-z0-9][A-Za-z0-9._-]{0,127})(?:\.git)?['"]?$`)

// shell is intended for an authorized_keys forced command. It turns a single
// SSH host into a safe Git remote without exposing the archive root path.
func shell(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: cryobank shell")
	}
	original := strings.TrimSpace(os.Getenv("SSH_ORIGINAL_COMMAND"))
	if fields := strings.Fields(original); len(fields) >= 2 && fields[0] == "cryobank" {
		return remote(fields[1:])
	}
	m := gitCommand.FindStringSubmatch(original)
	if m == nil {
		return errors.New("only Git upload and receive commands are allowed")
	}
	root, err := archiveRoot()
	if err != nil {
		return err
	}
	name := strings.TrimSuffix(m[2], ".git")
	repo := filepath.Join(root, name+".git")
	if m[1] == "git-receive-pack" {
		if _, err := os.Stat(repo); errors.Is(err, os.ErrNotExist) {
			if out, err := exec.Command("git", "init", "--bare", repo).CombinedOutput(); err != nil {
				return fmt.Errorf("cannot create repository: %w: %s", err, strings.TrimSpace(string(out)))
			}
		} else if err != nil {
			return err
		}
	} else if info, err := os.Stat(repo); err != nil || !info.IsDir() {
		return errors.New("repository does not exist")
	}
	cmd := exec.Command(m[1], repo)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	if m[1] == "git-receive-pack" {
		_ = exec.Command("git", "-C", repo, "config", "--unset-all", "cryobank.frozenAt").Run()
		_ = exec.Command("git", "-C", repo, "config", "cryobank.activeAt", time.Now().UTC().Format(time.RFC3339)).Run()
		setDefaultBranch(repo)
	}
	return nil
}

func setDefaultBranch(repo string) {
	if exec.Command("git", "-C", repo, "rev-parse", "--verify", "HEAD").Run() == nil {
		return
	}
	out, err := exec.Command("git", "-C", repo, "for-each-ref", "--format=%(refname)", "refs/heads/main", "refs/heads").Output()
	if err != nil {
		return
	}
	if refs := strings.Fields(string(out)); len(refs) > 0 {
		_ = exec.Command("git", "-C", repo, "symbolic-ref", "HEAD", refs[0]).Run()
	}
}

func refState(repo string) (string, error) {
	refs, err := exec.Command("git", "-C", repo, "show-ref").Output()
	if err != nil {
		return "", err
	}
	head, err := exec.Command("git", "-C", repo, "symbolic-ref", "-q", "HEAD").Output()
	if err != nil {
		// A detached HEAD is valid in a bare repository; include its object ID.
		head, err = exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
		if err != nil {
			return "", err
		}
	}
	lines := strings.Split(strings.TrimSpace(string(refs)), "\n")
	sort.Strings(lines)
	h := sha256.New()
	io.WriteString(h, strings.TrimSpace(string(head))+"\n")
	io.WriteString(h, strings.Join(lines, "\n")+"\n")
	return hex.EncodeToString(h.Sum(nil)), nil
}

func compareRefs(bundle, repo string) error {
	bundleOut, err := exec.Command("git", "bundle", "list-heads", bundle).Output()
	if err != nil {
		return fmt.Errorf("cannot read bundle refs: %w", err)
	}
	repoOut, err := exec.Command("git", "-C", repo, "show-ref").Output()
	if err != nil {
		return fmt.Errorf("cannot read archived refs: %w", err)
	}
	want := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(bundleOut)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.HasPrefix(fields[1], "refs/") {
			want[fields[1]] = fields[0]
		}
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(repoOut)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			got[fields[1]] = fields[0]
		}
	}
	if len(want) != len(got) {
		return errors.New("archived refs differ from uploaded bundle")
	}
	for ref, oid := range want {
		if got[ref] != oid {
			return fmt.Errorf("archived ref %s differs from uploaded bundle", ref)
		}
	}
	return nil
}

func serve(args []string) error {
	root, err := archiveRoot()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:9418", "HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Serving %s on http://%s\n", root, *listen)
	return http.ListenAndServe(*listen, webHandler(root))
}

type repoView struct {
	Name, Ref, Path, Content, Status string
	Repos                            []repoCard
	Files                            []string
	Branches                         []branchView
	Commits                          []commitView
}
type branchView struct{ Name, Ref string }
type commitView struct{ Hash, Subject, Date string }
type repoCard struct {
	Name, Status, Activity string
	When                   time.Time
}

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Cryobank</title>
<style>body{font:16px system-ui;max-width:72rem;margin:3rem auto;padding:0 1rem;color:#222}a{color:#075985}pre{background:#f4f4f5;padding:1rem;overflow:auto}li{margin:.35rem 0}.muted{color:#71717a}.feed{list-style:none;padding:0}.feed li{display:flex;gap:1rem;padding:.8rem 0;border-bottom:1px solid #e4e4e7}.feed a{font-weight:600;min-width:14rem}.status{margin-left:auto}.frozen{color:#0369a1}.deep-archive{color:#52525b}</style></head><body>
<h1><a href="/">Cryobank</a>{{if .Name}} / {{.Name}}{{end}}</h1>
{{if .Repos}}<h2>Projects</h2><ul class="feed">{{range .Repos}}<li><a href="/{{.Name}}/">{{.Name}}</a><span class="muted">{{.Activity}}</span><span class="status {{.Status}}">{{.Status}}</span></li>{{end}}</ul>{{end}}
{{if .Name}}{{if .Content}}<pre>{{.Content}}</pre>{{else}}
<p><a href="/{{.Name}}/tree?ref={{.Ref}}">Files</a> · <span class="{{.Status}}">{{.Status}}</span> · <span class="muted">read-only bare repository</span></p>
{{if .Files}}<ul>{{range .Files}}<li><a href="/{{$.Name}}/blob?ref={{$.Ref}}&path={{urlquery .}}">{{.}}</a></li>{{end}}</ul>{{end}}
{{if .Branches}}<h2>Branches</h2><ul>{{range .Branches}}<li><a href="/{{$.Name}}/tree?ref={{urlquery .Ref}}">{{.Name}}</a></li>{{end}}</ul>{{end}}
{{if .Commits}}<h2>Recent commits</h2><ul>{{range .Commits}}<li><code>{{.Hash}}</code> {{.Subject}} <span class="muted">{{.Date}}</span></li>{{end}}</ul>{{end}}
{{end}}{{end}}</body></html>`))

func webHandler(root string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) == 1 && parts[0] == "" {
			repos, err := listRepos(root)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			page.Execute(w, repoView{Repos: repos})
			return
		}
		name := parts[0]
		if !safeName.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		repo := filepath.Join(root, name+".git")
		if info, err := os.Stat(repo); err != nil || !info.IsDir() {
			http.NotFound(w, r)
			return
		}
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			ref = "HEAD"
		}
		if !safeRef.MatchString(ref) || strings.Contains(ref, "..") {
			http.Error(w, "invalid ref", 400)
			return
		}
		card, err := repositoryCard(root, name)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		view := repoView{Name: name, Ref: ref, Status: card.Status}
		mode := "summary"
		if len(parts) > 1 {
			mode = parts[1]
		}
		switch mode {
		case "", "summary":
			view.Commits, err = recentCommits(repo, ref)
			if err == nil {
				view.Branches, err = branches(repo)
			}
		case "tree":
			view.Files, err = treeFiles(repo, ref)
		case "blob":
			view.Path = r.URL.Query().Get("path")
			view.Content, err = blob(repo, ref, view.Path)
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		page.Execute(w, view)
	})
}

func listRepos(root string) ([]repoCard, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var repos []repoCard
	for _, entry := range entries {
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".git") {
			card, err := repositoryCard(root, strings.TrimSuffix(entry.Name(), ".git"))
			if err != nil {
				return nil, err
			}
			repos = append(repos, card)
		}
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].When.After(repos[j].When) })
	return repos, nil
}

func repositoryCard(root, name string) (repoCard, error) {
	repo := filepath.Join(root, name+".git")
	status := "active"
	var when time.Time
	if text, err := exec.Command("git", "-C", repo, "config", "--get", "cryobank.frozenAt").Output(); err == nil {
		when, _ = time.Parse(time.RFC3339, strings.TrimSpace(string(text)))
		status = "frozen"
		if !when.IsZero() && time.Since(when) >= 180*24*time.Hour {
			status = "deep-archive"
		}
	} else if text, err := exec.Command("git", "-C", repo, "config", "--get", "cryobank.activeAt").Output(); err == nil {
		when, _ = time.Parse(time.RFC3339, strings.TrimSpace(string(text)))
	}
	if when.IsZero() {
		text, err := exec.Command("git", "-C", repo, "log", "-1", "--all", "--format=%aI").Output()
		if err != nil {
			return repoCard{}, err
		}
		when, _ = time.Parse(time.RFC3339, strings.TrimSpace(string(text)))
	}
	activity := when.Local().Format("2006-01-02")
	if when.IsZero() {
		activity = "no activity"
	}
	return repoCard{Name: name, Status: status, Activity: activity, When: when}, nil
}

func branches(repo string) ([]branchView, error) {
	out, err := exec.Command("git", "-C", repo, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes").Output()
	if err != nil {
		return nil, err
	}
	var branches []branchView
	for _, ref := range strings.Fields(string(out)) {
		name := strings.TrimPrefix(ref, "refs/heads/")
		if name == ref {
			name = strings.TrimPrefix(ref, "refs/")
		}
		branches = append(branches, branchView{Name: name, Ref: ref})
	}
	return branches, nil
}

func recentCommits(repo, ref string) ([]commitView, error) {
	out, err := exec.Command("git", "-C", repo, "log", "-30", "--date=short", "--format=%h%x00%s%x00%ad", ref).Output()
	if err != nil {
		return nil, err
	}
	var commits []commitView
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) == 3 {
			commits = append(commits, commitView{f[0], f[1], f[2]})
		}
	}
	return commits, nil
}

func treeFiles(repo, ref string) ([]string, error) {
	out, err := exec.Command("git", "-C", repo, "ls-tree", "-r", "--name-only", ref).Output()
	if err != nil {
		return nil, err
	}
	files := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(files) == 1 && files[0] == "" {
		return nil, nil
	}
	return files, nil
}

func blob(repo, ref, path string) (string, error) {
	if path == "" || strings.ContainsRune(path, '\x00') {
		return "", errors.New("invalid path")
	}
	cmd := exec.Command("git", "-C", repo, "show", ref+":"+path)
	var out strings.Builder
	cmd.Stdout = io.MultiWriter(&limitedWriter{W: &out, N: 2 << 20})
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

type limitedWriter struct {
	W io.Writer
	N int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.N {
		return 0, errors.New("file is too large to display")
	}
	n, err := w.W.Write(p)
	w.N -= int64(n)
	return n, err
}
