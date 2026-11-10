# Third-party components

The [MIT License](../LICENSE) applies to a8s's own source and documentation.
Third-party software and downloaded Agent content retain their upstream terms.
This page identifies the components explicitly assembled by the runtime/example;
it is not a complete inventory of every transitive package in an image.

| Component | How a8s uses it | License source |
| --- | --- | --- |
| Upstream Codex 0.166.0 | Verified official runtime archive, including app-server and exec-server | [Pinned Apache-2.0 license](https://github.com/openai/codex/blob/rust-v0.166.0/LICENSE), copied to [images/licenses/codex-LICENSE](../images/licenses/codex-LICENSE) |
| Node 22.18.0 | Runtime binary and npm tooling from the pinned build image | [Versioned license and bundled dependency notices](https://github.com/nodejs/node/blob/v22.18.0/LICENSE), copied to [images/licenses/node-LICENSE](../images/licenses/node-LICENSE) |
| `frontend-design` Skill | Downloaded into the example Agent image at a fixed upstream commit | [Pinned Apache-2.0 license](https://github.com/anthropics/skills/blob/2c5958ecac7cee2b583967a2c4981d0f4a75c579/skills/frontend-design/LICENSE.txt), preserved by the Agentfile |
| `prettier` | Locked remote CLI dependency in the example | MIT, recorded with the exact version in [package-lock.json](../examples/frontend-assistant/src/package-lock.json) and included by npm |
| Go and TypeScript dependencies | Platform binaries and SDK implementation | Versions are recorded in `go.mod` / `go.sum` and `sdk/ts/pnpm-lock.yaml`; upstream packages retain their own licenses |
| Debian/system packages | Runtime tools installed by the Dockerfile | Package copyright/license files remain in the base image and `/usr/share/doc` |

The runtime image places a8s's license and the checked-in Codex/Node notices under
`/usr/share/licenses/a8s/`. The complete Codex distribution is retained rather
than copying only selected executable files, and npm's package directory is
copied with its included license files.

The example does not vendor the remote Skill into this source checkout. Its
Agentfile downloads both `SKILL.md` and `LICENSE.txt` from the pinned upstream
revision. Keep the license and supporting files when changing that dependency.
