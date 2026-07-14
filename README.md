# Cryobank

Push private Git repositories to your own machine, browse them, and freeze the
projects you no longer want on your Mac.

Cryobank has three executables:

- `cryobank` runs the SSH endpoint and web browser.
- `git-freeze` makes `git freeze` work.
- `git-thaw` makes `git thaw` work.

There are no users, pull requests, issues, actions, or database. Bare Git
repositories hold everything.

> [!CAUTION]
> `git freeze` ends by moving your checkout to the macOS Trash. Cryobank
> verifies the upload first, but start with a project you can afford to lose.

## Install

Install Go and Git, then build the three executables:

```sh
$ make test
$ sudo make install
```

Set `PREFIX` if `/usr/local` is not on your `PATH`:

```sh
$ make install PREFIX="$HOME/.local"
```

Install Cryobank on your Mac and the SSH host.

## Set up the host

Choose the directory that will hold the bare repositories:

```sh
$ cryobank init /Volumes/Tundra/Cryobank
Cryobank root configured at /Volumes/Tundra/Cryobank
```

Use a dedicated SSH key. Add it to `~/.ssh/authorized_keys` on the host with a
forced command:

```text
command="/usr/local/bin/cryobank shell",restrict ssh-ed25519 AAAA... cryobank
```

Add an SSH alias on your Mac:

```sshconfig
Host cryobank
    HostName pangolin.local
    IdentityFile ~/.ssh/id_ed25519_cryobank
```

Tell the Git commands which host to use:

```sh
$ git config --global cryobank.host cryobank
```

## Push and pull

The forced SSH command resolves repository names inside the configured root.
The first push creates the bare repository.

```sh
$ git remote add cryobank cryobank:weekend-compiler.git
$ git push -u cryobank main
$ git clone cryobank:weekend-compiler.git
```

## Freeze and thaw

Freeze the current checkout:

```sh
$ git freeze
Froze /Users/me/Code/weekend-compiler to cryobank as weekend-compiler and moved it to Trash.
```

Before touching the checkout, Cryobank:

1. Captures all refs, stashes, staged changes, unstaged changes, and untracked
   files that Git does not ignore.
2. Uploads a resumable Git bundle over SSH.
3. Verifies its checksum and every ref on the host.
4. Checks that the checkout did not change during the upload.
5. Calls the macOS `/usr/bin/trash` command.

Ignored files stay behind. They often contain builds, dependencies, caches, or
secrets and do not belong in Git storage.

Bring a project back with:

```sh
$ git thaw weekend-compiler
```

Thaw restores the branch, file contents, untracked files, and stash refs.
Staged and unstaged changes return as unstaged changes. Pass a second argument
to choose the checkout path:

```sh
$ git thaw weekend-compiler ~/Code/compiler
```

## Browse

Start the read-only web UI on the host:

```sh
$ cryobank serve
Serving /Volumes/Tundra/Cryobank on http://127.0.0.1:9418
```

Forward it to your Mac:

```sh
$ ssh -N -L 9418:127.0.0.1:9418 pangolin.local
```

Open `http://127.0.0.1:9418`. The project feed marks repositories as active,
frozen, or deep archive. A frozen project becomes deep archive after 180 days.
Repository pages show branches, recent commits, trees, and files.

> [!WARNING]
> The web UI has no authentication. Keep it on localhost and use an SSH tunnel,
> VPN, or trusted reverse proxy.

## Storage

The configured root contains ordinary bare repositories:

```text
Cryobank/
├── weekend-compiler.git/
├── old-homebrew.git/
└── .uploads/
```

Back up that whole directory. The `.uploads` directory contains resumable
transfers and can be discarded when no freeze is running.
