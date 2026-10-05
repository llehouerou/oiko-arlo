# oiko-arlo

Arlo cameras for [Oiko](https://github.com/llehouerou/oiko): the `arlo` type of Bridge,
added to an Oiko build like any other (Oiko's ADR 0017). It follows Arlo's cloud through
[go-arlo](https://github.com/llehouerou/go-arlo) (Oiko's ADR 0010); Arlo has no local API.

What it follows:

- each camera: its motion, as an `occupancy` Function (like a Zigbee motion sensor), its
  battery, a diagnostic Capability in %, and whether it is connected (its Availability);
- each base station: whether it is connected, and the location's mode, as the `mode`
  Capability of an `arming` Function. Oiko can set it to `standby`, `armHome` or
  `armAway`; a custom mode the owner activates in Arlo's app is shown as `custom`, and
  cannot be commanded.

## Configure

The Bridge's section of Oiko's configuration, under `bridges`:

```json
{"bridges": {"arlo": {
  "email": "oiko@example.com",
  "passwordFile": "/path/to/arlo-password",
  "imapServer": "imap.example.com:993",
  "imapUser": "oiko@example.com",
  "imapPasswordFile": "/path/to/imap-password"
}}}
```

- `email`, `passwordFile`: the Arlo account Oiko logs in as. Use a dedicated account the
  owner of the cameras granted access to, never the owner's. Passwords are read from files,
  so that they stay out of the configuration.
- `imapServer` (`host:port`, TLS), `imapUser`, `imapPasswordFile`: the mailbox where
  Arlo sends the account's two-factor codes, by email. The Bridge reads them over IMAP, so
  that logging in needs nobody.
- `dumpDir` (optional), a debugging aid: a directory receiving one JSON file per HTTP
  response and MQTT message from Arlo, secrets redacted, never rotated.

`email`, `imapServer` and `imapUser` are required.

### The session file

The Bridge keeps its Arlo session in `session.json` in its data directory,
`<data>/<its name>/`: `/var/lib/oiko/arlo/session.json` for a Bridge named `arlo` on NixOS.
Arlo rate limits logins with a long cooldown and rotates its trusted-browser cookie on every
login, so a session file belongs to one Oiko instance: **never copy it to another host**, and
leave it out of what a backup restores elsewhere. A second instance pairs its own session, at
the cost of one two-factor code. go-arlo alone decides when to log in again; restarting Oiko
in a loop only burns Arlo's rate limit.

## Add it to Oiko

Build an Oiko with this type (Go 1.27 needed):

```sh
go run github.com/llehouerou/oiko/cmd/oiko-build@v0.3.0 -with github.com/llehouerou/oiko-arlo@v0.1.0 -o oiko
./oiko -version   # lists the arlo type, with its module and version
```

On NixOS, override Oiko's package with this module's version, and pass the secrets through
`services.oiko.credentials`, which the service reads as
`/run/credentials/oiko.service/<name>`:

```nix
services.oiko.package = oiko.packages.${system}.default.override {
  bridges."github.com/llehouerou/oiko-arlo" = "v0.1.0";
  vendorHash = "sha256-…"; # the first nix build prints it
};
services.oiko.credentials = {
  arlo_password = "/run/secrets/arlo-password";
  arlo_imap_password = "/run/secrets/arlo-imap-password";
};
services.oiko.settings.bridges.arlo = {
  email = "oiko@example.com";
  passwordFile = "/run/credentials/oiko.service/arlo_password";
  imapServer = "imap.example.com:993";
  imapUser = "oiko@example.com";
  imapPasswordFile = "/run/credentials/oiko.service/arlo_imap_password";
};
```

See Oiko's README for building and deploying it.

## Versions

Versions follow Oiko's (its ADR 0019): during v0, a patch release neither breaks nor adds
anything, and a minor release may break the configuration, its notes listing what changed.
Each version's `go.mod` names the Oiko it needs.

## Develop

```sh
direnv allow   # or `nix develop`: Go 1.27
go test ./...
go run github.com/llehouerou/oiko/cmd/oiko-build@v0.3.0 -with github.com/llehouerou/oiko-arlo=. -o oiko
```
