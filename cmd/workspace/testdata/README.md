# PR #200 Pi bridge fixture

`pi-bridge-200.zip` contains the exact bridge pair from PR #200's merge commit,
`79836653f2d1c23d0feae12dadac14d3551e4300`. The command tests use this local
archive to qualify the released investigation bridge without depending on the
current bridge or on Git history.

To reproduce the archive from a checkout that has the merge commit:

```sh
git archive --format=zip --output=cmd/workspace/testdata/pi-bridge-200.zip \
  79836653f2d1c23d0feae12dadac14d3551e4300 \
  extensions/pi-bridge/index.ts extensions/pi-bridge/bridge.mjs
```

SHA-256 of the uncompressed files:

| File | SHA-256 |
| --- | --- |
| `index.ts` | `39d5528abee0b6b39d0088d265dbe4d51c9c00eb9f43fe1d4d6be9d2383a7a82` |
| `bridge.mjs` | `c6e571fbeed7b6f78ea1a8b7b044babca691923297d1803a3aaaeb5b6c6eeb2b` |
