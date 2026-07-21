# Phyhub L-IoT Device Provisioning

A device provisioning tool that runs before the user login. It collects the device's hardware facts (machine-id, platform identifiers, TPM, Secure Boot status, MAC addresses, …) and hands them to the local snapd, which then registers the device with the L-IoT Appstore.

Two flows are supported:

- **`claiming-token`**: the end user binds the device to their account by typing a console-displayed token into the L-IoT Appstore.
- **`basic`**: the device is pre-claimed (or no tenant binding is needed) and registers itself unattended.

If the device is already provisioned (snapd reports a serial assertion), the tool terminates immediately on start. The service unit can stay enabled after initial provisioning.

Internally the code is split into a reusable `sdk/` package and a thin `cmd/` binary that wires it together.


## Building

The version is read from the [`VERSION`](VERSION) file by default and embedded into the binary at build time via `-ldflags -X main.version=<ver>`.

```bash
# Default: linux/arm64, version from VERSION file
./build.sh

# Pick a target architecture
./build.sh amd64

# Override the version (e.g. for a release candidate or local snapshot)
./build.sh --override-version 1.2.0
./build.sh --override-version 1.2.0 amd64
```

Output goes to `bin/liot-provisioning-<version>-linux-<arch>`.

**Requirements:** Go 1.25+ (see [`go.mod`](go.mod))


## Integration

The intended deployment is a single static binary plus one of the provided systemd units.

### 1. Install the binary

The binary **must** be installed at exactly:

```
/usr/bin/liot-provisioning
```

The name and path are not arbitrary: snapd uses the existence of this exact file as the signal that the image ships an external L-IoT provisioning tool. If the file is missing (or installed under a different name or directory), snapd skips the `await-liot-registration-data` task in its registration change and runs `request-serial` immediately with its own minimal default payload.

This also matches the `ExecStart=` path in the shipped service files.

### 2. Install a service file

Two units are provided under [`deploy/yocto/`](deploy/yocto/), one per flow. Pick the one that matches your provisioning model. **Install only one**, not both:

| Unit | Flow | Use when |
|---|---|---|
| `phyhub-liot-device-provisioning-claiming-token.service` | `claiming-token` | The end user binds the device to their Appstore account by typing a token shown on the console. |
| `phyhub-liot-device-provisioning-basic.service` | `basic` | The device is pre-claimed (or no tenant binding is needed) and should register itself. |

The units are expected at:

```
/lib/systemd/system/phyhub-liot-device-provisioning-<flow>.service
```

and must be enabled so they auto-start on boot. The shipped units have `WantedBy=multi-user.target`, so the rootfs needs the matching symlink:

```
/etc/systemd/system/multi-user.target.wants/phyhub-liot-device-provisioning-claiming-token.service
  → /lib/systemd/system/phyhub-liot-device-provisioning-claiming-token.service
```

(a relative symlink works too). This is exactly what `systemctl enable` would create at runtime; placing it during image build has the same effect. systemd picks the unit up automatically on the next boot.

The shipped units can be used as-is or as templates:

- `ExecStart=/usr/bin/liot-provisioning <flow>`; the binary path is fixed (snapd gate, see above). The unit name is not.
- `Conflicts=getty.target` and the `ExecStartPre` getty-stop block take exclusive control of the console while running, then `ExecStopPost` re-starts `getty.target` so login prompts return after registration. This is board-agnostic (works on `ttymxc0`, `ttyS0`, `ttyAMA0`, …) but assumes a serial-console boot setup.
- `After=network-online.target snapd.service`; registration cannot proceed without snapd, and the Appstore requires network. Both must be reachable before the unit starts.
- `Restart=no`; the unit is one-shot. The binary itself terminates with exit 0 on subsequent boots once a serial assertion is present. 

## Flows

The first positional argument selects the flow.

```bash
liot-provisioning claiming-token   # claim token + poll Appstore + submit
liot-provisioning basic            # collect + submit, no token
```

Both flows talk to snapd over `/run/snapd.socket`, the binary must run as root. The shipped service units run as `User=root`.

### Flow: `claiming-token`

Used when the end user binds the device to their tenant via the L-IoT Appstore.

1. Waits for snapd to finish seeding.
2. Resolves the Appstore URL via snapd.
3. Loads or generates a claiming token (persisted across reboots in `/var/lib/snapd/claiming-token`).
4. Displays the token on the console and prompts the user to enter it in the Appstore.
5. Polls `/device/v3/provisioning/claim/status` until the token is claimed.
6. Collects device facts, builds the registration payload, submits to snapd.
7. Observes snapd's registration progress until a terminal state (registered / failed) and prints a final banner.

If the token expires before claiming completes, the local + remote partial state is cleared and the device reboots so the next attempt starts with a fresh token. The operator can also press `Alt+R` at any time to reset and reboot manually.

Typical console output:

```
[10:00:01] Snapd is installing initial applications...
[10:00:08] Snapd has installed initial applications

+--------------------------------------+
|     Enter this token in the L-IoT    |
|     Appstore to bind this device     |
|           to your account:           |
|                                      |
|            ABCD-1234-EFGH            |
+--------------------------------------+

  Press Alt+R at any time to reset the token and reboot.

[10:00:09] Waiting for you to enter the token in the L-IoT Appstore...
[10:14:12] Token accepted, registering device...
[10:14:31] Device registered (OS-Serial: 7f9b3c2a-...)
```

### Flow: `basic`

Used when the device is pre-claimed (or no tenant binding is needed) and should register itself on first boot.

1. Waits for snapd to finish seeding.
2. Collects device facts.
3. Builds the registration payload (no token).
4. Submits to snapd.
5. Observes snapd's registration progress until a terminal state and prints a final banner.

Typical console output:

```
[10:00:01] Snapd is installing initial applications...
[10:00:08] Snapd has installed initial applications
[10:00:32] Device registered (OS-Serial: 7f9b3c2a-...)
```

### Console output behaviour

Both flows emit a event log: one line per observable transition, in plain language. The console stays quiet between transitions. Recurring states (`pending`, `unreachable`, …) print **once** on the transition into that state, not on every poll.

If snapd gets stuck, the observer escalates after a grace period and dumps the relevant `snap changes` / task list as a diagnostic, e.g.:

```
[10:20:14] Still registering after 5m0s, diagnostic follows:
  Snap changes (in-progress):
    ID  Status   Summary
    42  Doing    Initialize device
    Tasks for change 42 (Initialize device):
    Status   Task
    Done     Generate device key
    Doing    Request device serial
```

**Snap warnings** are surfaced as soon as they appear, not only on escalation:
a snap warning always signals a problem (a blocked or failed install, a
store-contact failure, an assertion issue), and a blocked install frequently
leaves the seeding change sitting in `Doing` forever, so the warning is often
the only clue that the device will never finish on its own. Each warning is
printed once (deduplicated across polls):

```
[10:00:12] Snapd warning: cannot install "some-snap": snap is blocked
```

Warnings that match a known-critical keyphrase are tagged so operators (and log
scraping) can single them out:

```
[10:00:12] Snapd warning [CRITICAL]: cannot install "some-snap": snap is blocked
```

(The critical-phrase list is currently empty and will be populated as we learn
which messages reliably indicate an unrecoverable install.)

Reachability transitions are also surfaced:

```
[10:00:01] Cannot reach Snapd yet, retrying...
[10:00:23] Connection to Snapd restored
```

## What's submitted to snapd

Snapd accepts a partial v1 body and injects the snapd-owned fields (`format_version`, `nonce`, `snap.assertions_b64`, `attestation.tpm.*`) at registration time. The tool sends:

```json
{
  "claim":        { "token": "..." },
  "hardware":     { "machine_id": "...", "platform": { ... }, "tpm": { ... }, "secure_boot": { ... }, "network_interfaces": [ ... ] },
  "software":     null,
  "collector":    { "name": "liot-provisioning", "version": "1.2.0", "binary_sha256": "..." },
  "collected_at": "2026-04-26T10:00:00Z"
}
```

Hardware fields are best-effort: missing data is omitted. Software inventory is currently not yet populated by the tool.
