# macOS signing and notarization

Release disk images are signed with a **Developer ID Application** certificate and notarized by
Apple, so Gatekeeper opens them without the right-click → *Open* detour. `mygo.json` names the
identity and the `notarytool` keychain profile; `go tool mygo build` does the signing, submits the
`.dmg`, and staples the ticket to both the image and the `.app`.

Everything below is one-time setup per machine (or per CI repository). `scripts/macos-signing`
drives it and explains each missing piece:

```sh
scripts/macos-signing check
```

## Prerequisites

- A paid **Apple Developer Program** membership. A free Personal Team cannot issue Developer ID
  certificates, and notarization rejects its builds.
- Xcode or the Command Line Tools (`codesign`, `xcrun notarytool`, `xcrun stapler`).
- The certificate's private key. Generate it here: it never has to leave the machine, except as the
  base64 secret described below.

Team id and certificate are already recorded in `mygo.json`:

```json
"macos": {
  "signingIdentity": "Developer ID Application: xia li (X84WC7J5JH)",
  "notarize": { "keychainProfile": "mytoken-notary" }
}
```

Change both values if you sign with a different account.

## 1. Certificate

### Create the key and the signing request

```sh
scripts/macos-signing csr
```

This writes, refusing to overwrite anything already there:

- `~/apple-signing/DeveloperID.key` — the private key, mode `0600`
- `~/apple-signing/DeveloperID.certSigningRequest`

Back up the key somewhere safe. Losing it means revoking the certificate and issuing a new one.

### Issue the certificate

1. Open <https://developer.apple.com/account/resources/certificates/add>.
2. Choose **Developer ID Application** — not *Apple Development*, which Xcode issues for free.
3. Upload the `.certSigningRequest` and download the `.cer`.

An account can hold five Developer ID Application certificates; if one already exists, download and
reuse it instead of creating another.

### Install it

```sh
scripts/macos-signing install-cert ~/Downloads/developerID_application.cer
```

The certificate is imported into the default keychain next to its private key and copied to
`~/apple-signing/DeveloperID.cer`. Keychain Access should list it as valid; confirm with:

```sh
security find-identity -v -p codesigning
```

The first `codesign` run may ask for permission to use the key — choose **Always Allow**.

## 2. Notary credentials

Store an app-specific password in the keychain profile named in `mygo.json`
(`mytoken-notary`):

```sh
scripts/macos-signing credentials
# or non-interactively:
scripts/macos-signing credentials --apple-id you@example.com
```

It asks for an **app-specific password**, which you mint at
<https://account.apple.com> under *Sign-In and Security*; your Apple ID password is never stored.
An App Store Connect API key works too:

```sh
scripts/macos-signing credentials --key AuthKey_ABC123.p8 --key-id ABC123 --issuer 57246542-…
```

Check both halves at once:

```sh
make signing-check
```

## 3. Build

```sh
make dmg
```

This is the release build: universal `.app` and `.dmg`, signed with the Developer ID, notarized and
stapled. It fails with a pointer to this page when the certificate or the credentials are missing.

Other targets:

| Target | Signing | Notarization |
| --- | --- | --- |
| `make app` | Developer ID if one is installed, ad hoc otherwise | never |
| `make dmg` | Developer ID (required) | yes |
| `make dmg-local` | ad hoc | never |

Verify a finished artifact — signature, hardened runtime, timestamp, ticket and Gatekeeper:

```sh
scripts/macos-signing verify build/darwin-universal/mytoken-0.5.7.dmg
```

It mounts the image, inspects the `.app` inside, and prints one line per failed check, so an ad-hoc
build fails the same checks a real one must pass. MyGo staples the standalone `.app` (for updates)
but not the copy inside the image; the image's ticket covers that copy, and `verify` treats it that
way. `scripts/macos-signing verify path/to/mytoken.app` still requires the app's own ticket.

## 4. CI (GitHub Actions)

`.github/workflows/release.yml` imports the certificate and stores the notary credentials on the
macOS runner, then builds the same way. Add these repository secrets (or organization secrets with
`--org <org> --repos <repo>`):

```sh
scripts/macos-signing export-p12 --out DeveloperID.p12
base64 -i DeveloperID.p12 | gh secret set MACOS_CERTIFICATE
gh secret set MACOS_CERTIFICATE_PASSWORD   # the password from export-p12
base64 -i AuthKey_XXXXXX.p8 | gh secret set APPLE_API_KEY
gh secret set APPLE_API_KEY_ID             # the key ID in the filename
gh secret set APPLE_API_ISSUER             # the issuer ID, for Team API keys
rm DeveloperID.p12
```

An Apple ID works too: set `APPLE_ID`, `APPLE_TEAM_ID` and `APPLE_APP_PASSWORD` (the app-specific
password) instead of the three API-key secrets.

`export-p12` asks for a password; any strong one works, it only protects the file in transit. The
workflow imports the p12 into a temporary keychain, decodes the API key into a temporary file,
stores the `mytoken-notary` profile in the runner's login keychain (notarytool's default), and
deletes both temporary files and the keychain afterwards. The macOS job fails early, naming what is
missing, when a secret is absent — a release must not silently fall back to an ad-hoc build.

## Troubleshooting

**`security find-identity` shows no Developer ID.** The certificate and its private key must be in
the same keychain. `install-cert --keychain FILE` imports into a specific one; `--keychain-password`
also allows codesign to use the key non-interactively.

**`CSSMERR_TP_NOT_TRUSTED`.** The **Developer ID Certification Authority** intermediate is missing.
Keychain Access → *System Roots* should contain it; if not, download it from
<https://www.apple.com/certificateauthority/> and import it into the login keychain.

**`notarytool` says the profile does not exist.** `mygo.json` names `mytoken-notary`; store the
credentials under that exact name, or change both places together.

**`Invalid signature` or `code object is not signed at all` inside the `.app`.** Something in the
bundle changed after signing (a helper, a resource). Rebuild from clean; the build signs the app
after every embedded binary is in place.

**The `.dmg` is notarized but the app opens with a warning.** The app inside the image must be
stapled too — `go tool mygo build` does this after a successful submission. Check with
`scripts/macos-signing verify *.app`.

**Notarization fails with `Signature Invalid`.** The certificate is an *Apple Development* one, or
the build was signed ad hoc. `scripts/macos-signing verify` names the identity it found.

**A free Personal Team account.** It cannot create Developer ID certificates. Enrol in the paid
program first.
