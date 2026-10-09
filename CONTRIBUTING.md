# Contributing

Build and test it (Go 1.27; `nix develop` gives it):

```sh
go vet ./...
go test ./...
go run github.com/llehouerou/oiko/cmd/oiko-build@latest -with github.com/llehouerou/oiko-arlo=. -o oiko
```

The tests run against a fake Arlo client and a fake S3 in memory, with no network and no real
clock: no Arlo account is needed. Test data uses made-up names and IDs, never a real account's.

Everything else follows Oiko's [contributing guide](https://github.com/llehouerou/oiko/blob/main/CONTRIBUTING.md):
what's welcome, when an issue comes first, the pull request terms, the AI policy. Everyone
follows Oiko's [code of conduct](https://github.com/llehouerou/oiko/blob/main/CODE_OF_CONDUCT.md).

Each minor release of Oiko ends with this repository moved to it and tagged.
