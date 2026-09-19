---
title: "SAM Connect (Android and iOS)"
linkTitle: "SAM Connect"
weight: 3
aliases:
  - /docs/user/mobile-app/
  - /docs/development/mobile/
---

{{% alert title="Preview" color="warning" %}}
The app is functional and tested in CI on an Android emulator. Its screens,
the on-device tools it exposes and the FFI surface may change.
{{% /alert %}}

SAM Connect is a Flutter app that runs `sam-node` on a phone. The Go node is
compiled into a shared library and driven over Dart FFI. The app is the UI
around it, plus a small MCP server that exposes the phone's sensors to the
mesh. A phone enrolled this way is a node like any other. It has a key, a
credential and a peer ID, and it appears in discovery with its
`phone-sensors` service.

## Using it

### Enroll

The first screen offers three ways to enroll:

- **Scan enrollment code**: point the camera at the `sam://enroll?...` QR
  code that `sam-one` prints at start, or that `sam-one token qr` creates.
  The app shows which control plane the code leads to before it uses the
  token. The phone's stock camera app opens the same link.
- **Enter details manually**: paste a control plane URL and a bootstrap
  token.
- **Login & Enroll** / **Device Login**: for a control plane with an identity
  provider, log in through the browser or with a device code.

The control plane URL must be `https`. The app accepts plaintext only for
`localhost`.

### Start

The **Start** button on the dashboard launches the node as a foreground
service, so it stays connected while the phone is idle. The dashboard shows
the peer ID (labelled **Node ID**), the connected peers and the DHT size.

### Services

The Services tab switches the built-in sensors on and off: **Battery
Status** (`get_battery_status`: level and charging state) and **Location**
(`get_location`: coordinates, with the OS permission). Enabled sensors are
published as the `phone-sensors` MCP service.

### Config

The Config tab holds what `sam-node.yaml` holds on a desktop: labels
(comma-separated `key=value`) and the attenuation rules, policies and checks
(one Datalog statement per line, with the same syntax and the same errors as
the file). Labels are attested at enrollment, so changing them requires
pressing **Re-enroll**. Re-enrollment uses the stored OIDC refresh token or
bootstrap identity and keeps the peer ID. The tab also shows the API token
that protects the node's local API on the phone.

On Android 16 the app also registers two AppFunctions, `getMeshStatus` and
`callRemoteMeshTool`, so an on-device assistant can use the mesh without the
app in the foreground.

## Calling the phone from elsewhere

From any enrolled node, the phone is a peer with an MCP service:

```bash
mcp-client -url "http://127.0.0.1:8080/sam/<phone-peer-id>/mcp/phone-sensors" -token "$TOKEN" -list
mcp-client -url "http://127.0.0.1:8080/sam/<phone-peer-id>/mcp/phone-sensors" -token "$TOKEN" -tool get_battery_status
```

```json
{"battery_level": 64, "charging": true}
```

An agent finds the phone in the usual way. `find_remote_tools` lists
`mcp://phone-sensors/get_location`, `describe_remote_tool` shows that it
takes no arguments, and `call_remote_tool` returns
`{"latitude": ..., "longitude": ...}`. The policy on the control plane
decides who may call it. The phone's own attenuation rules can narrow that
further.

## How it is built

```text
Flutter app (mobile/sam-node-app)
  lib/main.dart      the UI; talks to the node's local API over 127.0.0.1
  lib/sam_ffi.dart   Dart FFI wrapper
        │ C calls
Go FFI library (mobile/sam-node-ffi)
  StartNode, StopNode, EnrollNode, EnrollNodeBootstrap, ReEnrollNode,
  UnenrollNode, IsEnrolled, GetNodeID, GetMeshInfo, CallRemoteTool, ...
        │
sam-node (internal/node), unchanged
```

The FFI package is a thin export layer over the same node code that the CLI
uses, including `CompleteNodeConfig` for labels and attenuation, so both
agree on validation. Lifecycle operations go over FFI. Everything else the
UI does (listing peers, toggling services) goes over HTTP to the node's
loopback API with a token generated at each launch.

Make targets:

| Target | Output |
|---|---|
| `make mobile-ffi-host` | `bin/libsam.so` for the build host, for desktop tests of the FFI. |
| `make mobile-ffi-android` | `bin/android/libsam.so` (arm64-v8a). |
| `make mobile-ffi-android-x86_64` | `bin/android-x86_64/libsam.so`, for x86_64 emulators. Emulators on Apple Silicon run arm64 images and use the previous target. |
| `make mobile-ffi-ios` | `bin/ios/libsam.a`. |
| `make mobile-app-apk` | The FFI library copied into `jniLibs/` and a release APK. `mobile-app-apk-emulator` and `mobile-app-bundle` are the emulator and Play Store variants. |

For an edit-run loop on a device:

```bash
make mobile-ffi-android
mkdir -p mobile/sam-node-app/android/app/src/main/jniLibs/arm64-v8a
cp bin/android/libsam.so mobile/sam-node-app/android/app/src/main/jniLibs/arm64-v8a/
cd mobile/sam-node-app && flutter run
```

Release builds read signing material from `ANDROID_KEYSTORE_PATH`,
`ANDROID_KEYSTORE_PASSWORD`, `ANDROID_KEY_ALIAS` and `ANDROID_KEY_PASSWORD`,
and Firebase configuration from `GOOGLE_SERVICES_JSON` or its base64 form.
`mobile/sam-node-app/README.md` covers the toolchain setup.

## Testing

`mobile/mobile_e2e.sh` runs the end-to-end check that CI uses. It starts a
mock identity provider, a control plane, a router and a host node with a
test tool. It then boots an Android emulator with the app, enrolls it,
registers a tool inside the emulator, and checks that each side discovers
the other's tool through the mesh.

The app's privacy policy, for the store listing, is at
[/docs/privacy/](../../privacy/).
