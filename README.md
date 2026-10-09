# oiko-arlo

Arlo cameras for [Oiko](https://github.com/llehouerou/oiko): the `arlo` type of Bridge, added
to an Oiko build like any other. It follows Arlo's cloud through
[go-arlo](https://github.com/llehouerou/go-arlo)
([ADR 0010](https://github.com/llehouerou/oiko/blob/main/docs/adr/0010-arlo-through-go-arlo.md));
Arlo has no local API. It is the reference type for cameras and Recordings in Oiko's guide,
[Write a type of Bridge](https://github.com/llehouerou/oiko/blob/main/docs/write-a-bridge.md#cameras-and-recordings).

What it follows:

- each camera: its motion, as an `occupancy` Function (like a Zigbee motion sensor), its
  battery, a diagnostic Capability in %, whether it is connected (its Availability), and
  a `camera` Function: Oiko's dashboard shows its Picture, the latest image Arlo keeps
  (read without waking the camera), and opens its Live view on a tap, capped at 5 minutes
  since the camera runs on battery. The camera's History lists its Recordings, the videos
  in Arlo's library (kept by Arlo up to 31 days), which Members play from Oiko. Each new
  recording is announced as an Event of the `recording` Capability of the `camera`
  Function, as of the recording's start, its data what triggered it (`person`,
  `vehicle`, `animal`, `package`, `motion`, `sound`, or `other`): an Automation can
  send its video on. A reconnection announces the recordings of the last 15 minutes it
  missed, nothing older;
- each base station: whether it is connected (its Availability), and the location's mode,
  as the `mode` Capability of an `arming` Function. Oiko can set it to `standby`, `armHome`
  or `armAway`; a custom mode the owner activates in Arlo's app is shown as `custom`, and
  cannot be commanded.

The Bridge is online while Arlo's event stream is connected. Its Replay ends once the
location's mode is known, the cameras' state following it within a second or two.

## Arlo setup

- **A dedicated Arlo account.** In Arlo's app, the owner of the cameras grants an account of
  its own to Oiko (Settings › Grant Access), never the owner's own account. The account must
  see exactly one Arlo location holding its base stations; otherwise the Bridge never
  connects.
- **Two-factor codes by email.** The account receives its two-factor codes by email, in a
  mailbox the Bridge reads over IMAP (TLS), so that logging in needs nobody. Arlo asks for a
  code on the first login, and when it takes the session for an untrusted browser (error
  9261); the Bridge reads it in about 10 seconds.

## Configure

The Bridge's section of Oiko's
[configuration](https://github.com/llehouerou/oiko/blob/main/docs/configure.md#bridges),
under `bridges`:

```json
{"bridges": {"arlo": {
  "email": "oiko@example.com",
  "passwordFile": "/path/to/arlo-password",
  "imapServer": "imap.example.com:993",
  "imapUser": "oiko@example.com",
  "imapPasswordFile": "/path/to/imap-password"
}}}
```

- `email`, `passwordFile`: the dedicated Arlo account, and the file holding its password.
- `imapServer` (`host:port`, TLS), `imapUser`, `imapPasswordFile`: the mailbox where Arlo
  sends the account's two-factor codes, and the file holding its password.
- `dumpDir` (optional), a debugging aid: a directory receiving one JSON file per HTTP
  response and MQTT message from Arlo, secrets redacted, never rotated.

Every key but `dumpDir` is required. Passwords are read from the files the `…File` keys name,
so that they stay out of the configuration. Any other key stops Oiko from starting.

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
go run github.com/llehouerou/oiko/cmd/oiko-build@latest -with github.com/llehouerou/oiko-arlo@latest -o oiko
./oiko -version   # lists the arlo type, with its module and version
```

On NixOS, override Oiko's package with this module's version, and pass the secrets through
`services.oiko.credentials`, which the service reads as
`/run/credentials/oiko.service/<name>`:

```nix
services.oiko.package = oiko.packages.${system}.default.override {
  bridges."github.com/llehouerou/oiko-arlo" = "<version>"; # its latest tag
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

Its entry in the [catalogue](https://llehouerou.github.io/oiko-catalogue/#arlo) gives both,
with the latest versions. Oiko's [Install](https://github.com/llehouerou/oiko/blob/main/docs/install.md)
and [Configure](https://github.com/llehouerou/oiko/blob/main/docs/configure.md#add-a-type-of-bridge)
pages say the rest.

## Versions

Versions follow Oiko's
([ADR 0019](https://github.com/llehouerou/oiko/blob/main/docs/adr/0019-release-and-compatibility-policy.md)):
during v0, a patch release neither breaks nor adds anything, and a minor release may break
the configuration, its notes listing what changed. Each version's `go.mod` names the
minimum Oiko it needs.

## Develop

```sh
go test ./...
go run github.com/llehouerou/oiko/cmd/oiko-build@latest -with github.com/llehouerou/oiko-arlo=. -o oiko
```

The tests run against a fake Arlo client and a fake S3 in memory, with no network and no real
clock. `nix develop` gives Go 1.27. [CONTRIBUTING.md](CONTRIBUTING.md) says how to contribute,
[SECURITY.md](SECURITY.md) how to report a vulnerability.

## License

Apache-2.0: see [LICENSE](LICENSE) and [NOTICE](NOTICE). go-arlo, which it depends on, is MIT.
