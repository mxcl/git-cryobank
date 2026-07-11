# Git Attic

Git Attic moves finished projects to a bare Git repository on another Mac or
Unix host, exposes them through a small read-only website, and only then moves
the local working tree to the macOS Trash.

It is one Go binary with no runtime dependencies beyond Git, SSH, and macOS's
`trash` command on the client.

## Why the command is `git attic`

Git already owns `git archive`; it creates tar/zip snapshots and does not allow
an external `git-archive` helper to replace it. Git Attic therefore installs as
`git-attic`, which Git automatically exposes as:

```sh
git attic HOST [PATH]
```

`PATH` defaults to the repository containing the current directory.

## Install

Build the same binary on the client and server (or copy a compatible build):

```sh
go install github.com/mxcl/git-attic@latest
```

During local development:

```sh
go build -o git-attic .
install git-attic ~/bin/git-attic
```

`git-attic` must be on the non-interactive SSH `PATH` on the server. Confirm
that before archiving anything:

```sh
ssh archive-host 'git-attic --help'
```

## Configure and run the server

On the archive host, point Git Attic at the mounted external volume once:

```sh
git-attic init /Volumes/Archive/git
git-attic serve --listen 127.0.0.1:8080
```

`init` records the absolute root in `~/.config/git-attic/root`, so web serving
and non-interactive SSH receiver commands use the same directory. The
`GIT_ATTIC_ROOT` environment variable overrides that file when needed.

Open <http://127.0.0.1:8080>. To expose it to the LAN, bind an appropriate
interface, for example `--listen 0.0.0.0:8080`; the initial web UI has no HTTP
authentication, so use a firewall, VPN, or reverse proxy on untrusted networks.

The stored repositories are ordinary bare mirrors. Normal Git-over-SSH still
works without a custom protocol:

```sh
git clone archive-host:/Volumes/Archive/git/project.git
git push archive-host:/Volumes/Archive/git/project.git main
```

## Archive a project

From its working tree:

```sh
git attic archive-host
```

Or name a repository explicitly:

```sh
git attic archive-host ~/src/old-project
```

The directory basename becomes the remote name. Names are restricted to ASCII
letters, digits, `.`, `_`, and `-`; an existing name with different content is
never overwritten.

The client performs these steps:

1. Resolve the working-tree root and require a valid `HEAD`.
2. Reject staged changes, worktree changes, and non-ignored untracked files.
3. Create a Git bundle containing every ref (`--all`) and hash it with SHA-256.
4. Resume any partial upload with the same hash over SSH.
5. Ask the server to hash the complete bundle, clone it as a bare mirror, and
   compare every archived ref with the bundle.
6. Require the server's exact content-hash confirmation.
7. Invoke `/usr/bin/trash` on the local repository root.

If SSH drops, verification fails, the name collides, or Trash itself fails,
the local repository remains where it is. Retrying is safe: partial uploads
resume, and a previously verified archive with the same hash is idempotent.

Ignored files are not part of Git and are not archived; like the rest of the
working directory, they move to Trash after verification.

## Web UI

The standard-library HTTP server provides a repository index, recent commits,
a complete file list, and text blob viewing. It is deliberately read-only and
has no database, users, issues, pull requests, or background indexing.

## Development

```sh
go test -race ./...
go vet ./...
```
