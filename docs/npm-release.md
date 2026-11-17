# Platform-specific npm releases

The launcher package remains `@grokbuildx/gbx`. Its exact-version optional dependencies each contain one native binary, with npm `os` and `cpu` metadata selecting the current platform:

| Package suffix | Release asset |
| --- | --- |
| `darwin-x64` | `gbx-darwin-amd64.gz` |
| `darwin-arm64` | `gbx-darwin-arm64.gz` |
| `linux-x64` | `gbx-linux-amd64.gz` |
| `linux-arm64` | `gbx-linux-arm64.gz` |
| `win32-x64` | `gbx-win32-x64.exe.gz` |
| `win32-arm64` | `gbx-win32-arm64.exe.gz` |

Each suffix is prefixed with `@grokbuildx/gbx-`. The launcher contains no native binaries: its `gbx` command is the `bin/gbx.exe` placeholder, which the `bin/install.js` postinstall replaces with a hard link to (or copy of) the platform binary. The installed command therefore runs without Node.js.

The placeholder has no shebang, so npm's Windows shim executes the placed binary directly. When postinstall did not run (`--ignore-scripts`, pnpm or bun without trusted scripts, `--omit=optional`), Unix shells run the placeholder with `sh`, which hands off to `node bin/wrapper.js`; `node bin/install.js` places the binary afterwards. Older bundled installations are still recognized by the wrapper.

## Publishing credentials

The release workflow publishes with the repository secret `NPM_TOKEN`, exposed to npm as `NODE_AUTH_TOKEN`. It must be a granular access token with:

- Packages and scopes: Read and write on the `grokbuildx` organization scope (not selected packages, so new packages can be created).
- Bypass two-factor authentication enabled.
- No IP restriction (GitHub-hosted runners have no fixed address).

npm caps write-capable granular tokens at a limited lifetime, so rotate the token before it expires:

```bash
gh secret set NPM_TOKEN -R grokbuildx/gbx   # paste the new token when prompted
```

The npm job keeps `id-token: write`, so `npm publish --provenance` still records build provenance. Once all seven packages exist, npm Trusted Publishing can replace the token: configure each package with

```bash
npx -y npm@latest trust github @grokbuildx/<package> --file release.yml --repository grokbuildx/gbx --allow-publish -y
```

then drop `NODE_AUTH_TOKEN` from the workflow and delete the secret. `npm trust` needs npm 11.15 or newer, an existing package, and a browser 2FA confirmation per call.

## Release workflow

`node scripts/build-npm-packages.js vMAJOR.MINOR.PATCH` validates all six compressed release assets under `dist/` and stages seven packages under `dist/npm/`. Legacy four-component tags become npm prerelease versions, matching the previous workflow.

`node scripts/publish-npm-packages.js dist/npm` skips versions that already exist, publishes the six platform packages in parallel, and publishes the launcher once npm has accepted all six. It does not wait for npm to serve them: npm can take from about one to more than thirteen minutes per package, and an install in that window skips the unresolved optional dependency without failing. `bin/install.js` then downloads the same `.gz` asset from the GitHub release (`GBX_RELEASE_DOWNLOAD_URL` overrides the base URL), verifies it against `SHA256SUMS`, and places it; the npm job runs after the GitHub release job for that reason. A failed download fails the install. Reruns safely continue from already-published versions.

The launcher declares `engines.node >=22`.

`node --test scripts/npm-packages.test.js` checks the package layout, supported targets, binary resolution, postinstall placement, the GitHub release download when npm skipped the platform package, the placeholder fallback, legacy installs, CLI argument forwarding and exit codes. Packaging checks do not publish anything or require registry credentials.
