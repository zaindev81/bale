# bale

A Node.js version manager written in Go, with an optional minimal npm package
manager under `bale pkg`.

## Build

```sh
go build -o bale .
```

## Manage Node.js

```sh
./bale list-remote          # available releases for this OS and architecture
./bale list-remote 24       # available 24.x releases, newest first
./bale install 24           # install the latest available 24.x release
./bale use 24               # select the newest installed 24.x release

eval "$(./bale env)"        # configure PATH in the current sh/bash/zsh shell
node --version
npm --version

./bale list                 # installed releases; * marks the selected release
```

`install` and `list-remote` accept a major (`24`), major.minor (`24.10`),
exact version (`24.10.0` or `v24.10.0`), `latest`, or `lts`. Partial versions
resolve to the newest matching release; `lts` selects the newest LTS release
when installing and lists all LTS releases with `list-remote`.
`use` accepts numeric versions or `latest` and selects only from installed
versions, without accessing the network. `i` and `ls` alias `install` and `list`.

Installing does not change the selected version. Reinstalling an already
installed exact version works offline; partial versions and aliases check the
release index for newer versions. An unavailable version returns an error with
no version installed. The release index is https://nodejs.org/dist/index.json.

## Shell setup and storage

The default installation directory is `~/.bale`. Set `BALE_HOME` to an absolute
path to use a different directory. Versions live in `$BALE_HOME/versions/vX.Y.Z`;
`use` atomically updates `$BALE_HOME/current`. `bale env` prints shell code that
puts `current/bin` first on PATH, so `node`, `npm`, and `npx` come from the selected
Node.js distribution. Run it with `eval` as shown above.

To enable this in new terminals, add the following to `~/.zshrc` (or `~/.bashrc`),
replacing the example path with the absolute path to your built binary:

```sh
eval "$(/absolute/path/to/bale env)"
```

Place this after any other tool that modifies PATH, including other Node.js
version managers. If you change `BALE_HOME`, export it before this line. The
selection is shared by all shells using the same `BALE_HOME`; this is not a
per-shell selection. If the shell cached a previous `node` executable before
the first selection, run `eval "$(./bale env)"` again to refresh PATH and its cache.
The program does not edit startup files or system Node.js installations.

Supported platforms: macOS and Linux (glibc), each on arm64 or x64. Node.js
releases must provide a matching official binary; this tool does not compile
Node.js from source. Windows, musl Linux, automatic project switching, and
uninstall are not implemented.

Downloads use HTTPS from nodejs.org and are checked against that release's
`SHASUMS256.txt` before extraction. Archives are staged, bounded in size, and
checked for unsafe paths and symlinks. The bundled npm/npx links are preserved.
Checksum verification relies on the HTTPS distribution server; release signing
keys are not independently verified.

## Existing package-manager users

The top-level `install` and `list` commands now manage **Node.js itself**.
The previous npm dependency operations are available as `bale pkg install`,
`bale pkg list`, and `bale pkg init`. Existing `package.json` and `node_modules`
continue to be used by these commands.

## npm package commands

```sh
bale pkg init                # create a new package.json in the current directory
bale pkg install             # install everything in package.json (dependencies + devDependencies)
bale pkg install <pkg>...    # resolve and add one or more packages, e.g. `bale pkg install is-odd@^3.0.0`
bale pkg i <pkg>...          # alias for `bale pkg install`
bale pkg list                # show direct dependencies, installed versions, and problems
bale pkg ls                  # alias for `bale pkg list`
bale pkg help                # show usage
```

Set `BALE_REGISTRY` to point at a different registry (defaults to
`https://registry.npmjs.org`).

`bale pkg list` reads local files only and includes both dependencies and
devDependencies, sorted by name within each group. It shows the requested
range, installed version, and status, and exits with code 1 if a dependency
is missing, invalid, or incompatible. Dist-tags are marked as unchecked
because checking them requires the registry. Transitive and undeclared
packages are not included.

### npm package security

Installing a package means running someone else's archive through your
filesystem, so `bale` is deliberately strict:

- **Package names are validated** before they are used to build any
  filesystem path or registry URL — whether they were typed on the command
  line, read from `package.json`, or read from registry metadata. Names
  that could escape `node_modules` (`.`, `..`, `/`, absolute paths) or
  that collide with `node_modules` itself are rejected.
- **Tarballs are only fetched from the registry's own host** — the same
  host as `BALE_REGISTRY`, over that registry's scheme or https — and
  redirects are limited to that host and capped in number.
- **Integrity is required.** Tarballs are verified against the `integrity`
  field (sha512, sha384 or sha256); the legacy sha1 `shasum` is never
  trusted, and a package version without usable integrity data is refused.
- **Everything is bounded**: download size, packument size, unpacked size
  and archive entry count all have limits, so a malicious archive cannot
  fill the disk.
- **Archives never create symlinks**, entries that would escape the
  destination are rejected, and **no lifecycle scripts run** — nothing
  from a downloaded package is executed.
- **Installs are atomic.** A package is unpacked into a staging directory
  inside `node_modules`, checked (its `package.json` must parse and report
  the expected version), and only then renamed into place. The whole tree
  is resolved before anything is written, so a package that cannot be
  found fails the install with `node_modules` untouched. A failure while
  downloading or unpacking reports how many packages were installed; the
  next `bale pkg install` reuses those and continues from there.

### npm package limitations

This is a small, educational package manager, not a production tool:

- Flat, first-wins `node_modules` layout only — no nested resolution, no
  lockfile, no dedupe beyond "first installed version wins".
- Only registry (semver range or dist-tag) dependency specs are supported.
  Git, `http(s)`, `file:`, `link:`, `npm:`, and GitHub shorthand specs are
  rejected with an error.
- Mirrors that serve tarballs from a different host than the registry
  itself are not supported yet: such a tarball URL is refused.
- No lifecycle scripts (`postinstall`, etc.) are run.
- No `package-lock.json` is read or written.
- `bale pkg install <pkg>` always re-resolves and reinstalls the named packages
  (like `npm install foo`), while transitive dependencies already present in
  node_modules are reused as-is (first-wins). `bale pkg install` with no
  arguments repairs missing transitive packages and replaces direct
  dependencies (including devDependencies) whose installed versions do not
  satisfy the semver ranges in `package.json`. Matching installed versions
  are reused without checking for newer releases. Installed dependencies
  specified by dist-tag (such as `latest`) are also reused; use
  `bale pkg install <pkg>@<tag>` to refresh them from the registry.
