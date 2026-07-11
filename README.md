# `git-cryobank`

Archive old Git repositories on another machine, browse them on the web, then
put the local checkout in the macOS Trash.

> [!CAUTION]
> This is new software whose final step is moving a directory to Trash. It is
> deliberately paranoid, but perhaps don't begin with the source code to your
> livelihood.

## Quickstart

Install it on your Mac and the archive host:

```sh
$ brew install mxcl/made/git-cryobank
```

Choose the host once on your Mac:

```sh
$ git-cryobank target pangolin
Cryobank target configured as pangolin
```

> [!IMPORTANT]
> Mount the external disk before configuring its path. `/Volumes/Tundra` must
> actually be Tundra, not an unfortunately named directory on your boot disk.

Point the host at some durable storage and start the browser:

```sh
$ ssh pangolin 'git-cryobank init /Volumes/Tundra/Attic'
Cryobank root configured at /Volumes/Tundra/Attic

$ ssh pangolin 'git-cryobank serve'
Serving /Volumes/Tundra/Attic on http://127.0.0.1:9418
```

Then freeze a project:

```sh
$ cd ~/Developer/finished-with-this
$ git cryobank
Archived /Users/mxcl/Developer/finished-with-this to pangolin as finished-with-this and moved it to Trash.
```

The bare repository now lives at
`/Volumes/Tundra/Attic/finished-with-this.git`. Browse it through an SSH
tunnel:

```sh
$ ssh -N -L 9418:127.0.0.1:9418 pangolin
# ^^ open http://127.0.0.1:9418
```

## It either verifies or it doesn't touch your checkout

`git-cryobank` refuses repositories with staged changes, worktree changes, or
non-ignored untracked files. It bundles every ref plus `HEAD`, uploads over
SSH, verifies the SHA-256 and every ref on the host, then checks that nothing
local changed during the upload.

Only then does it call `/usr/bin/trash`.

Interrupted uploads resume. Repeating a completed upload is safe. A basename
collision, changed remote, failed verification, lost SSH connection, or failed
Trash operation leaves your local checkout where it is.

> [!NOTE]
> Git already owns `git archive`, so the command is `git cryobank`. Git finds
> the installed `git-cryobank` executable automatically.

## Choose the cryobank

`git-cryobank target HOST` writes the client destination to
`~/.config/git-cryobank/target`. `git-cryobank init DIR` writes the server
storage location to `~/.config/git-cryobank/root`. There are no flags,
environment variables, or fallback locations; missing configuration is an
error.

> [!WARNING]
> Ensure external storage is mounted before starting the server. Also, the web
> UI has no authentication; bind to localhost and use an SSH tunnel, VPN, or
> trusted reverse proxy.

## Ordinary Git remains ordinary Git

Repositories are bare mirrors, not entries in a database. Clone or push them
with normal Git-over-SSH:

```sh
$ git clone pangolin:/Volumes/Tundra/Attic/finished-with-this.git
$ git push pangolin:/Volumes/Tundra/Attic/finished-with-this.git main
```

The web UI is read-only and intentionally small: repositories, branches,
commits, file trees, and blobs. No users, issues, pull requests, actions, or other empty
furniture.

For the rest:

```sh
$ git-cryobank --help
```

## Build

Requires Go and Git:

```sh
$ go test -race ./...
$ go build ./...
```

The client requires macOS because it uses the native Trash. The receiver and
web server also build on Linux.
