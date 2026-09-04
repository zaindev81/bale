# bale

A minimal Node package manager written in Go. `bale` reads and writes
`package.json`, resolves dependencies against the npm registry, and
installs them into a flat `node_modules` directory.

## Build

```sh
go build -o bale .
```

## Usage

```sh
bale init                # create a new package.json in the current directory
bale install             # install everything in package.json (dependencies + devDependencies)
bale install <pkg>...    # resolve and add one or more packages, e.g. `bale install is-odd@^3.0.0`
bale i <pkg>...          # alias for `bale install`
bale help                # show usage
```

Set `BALE_REGISTRY` to point at a different registry (defaults to
`https://registry.npmjs.org`).

## Security

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
  next `bale install` reuses those and continues from there.

## Limitations

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
- `bale install <pkg>` always re-resolves and reinstalls the named packages
  (like `npm install foo`), while transitive dependencies already present in
  node_modules are reused as-is (first-wins). `bale install` with no
  arguments now verifies that the dependency tree is *complete* — a
  transitive package missing from `node_modules` is reinstalled — but it
  still does not check the versions already installed against
  `package.json`, because there is no lockfile to detect drift.
