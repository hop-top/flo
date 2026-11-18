# dek contract schemas

Vendored, unmodified, from hop-top/dek, branch `next`, commit
`af84c9b917f90731c1f370c0b05080fb45624506`, path `contract/schemas/`
(last changed in `c7fefc37af24`).

| File | sha256 |
|---|---|
| `Job.json` | `e6b1a09f7c885aba940e2b08964b3a6215e3365f7b5bfda026f991952ebef714` |
| `Plan.json` | `ccbdfde6cd9b3b6a3121079939dadfa5580f90edb061e0481edd04f737441af9` |
| `Proposal.json` | `b95f6c8f1438022aabe5cb449da624cd8341664af86eb28ff4bb1c13cac592e5` |

`internal/records` projects flo's records onto these shapes and its tests
validate the projections against them. To update: copy the three files from
the new dek commit, change the commit above, rerun `go test ./internal/records`.
