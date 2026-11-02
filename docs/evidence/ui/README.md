# Implemented UI evidence

Captured on **2026-11-02** from the real Go-embedded React UI and authenticated API at source revision `f3b2161`. The panel used new disposable local state on macOS, not HTTP mocks or production data. No Minecraft instance was provisioned. The test panel was stopped afterward.

- [Desktop account settings](desktop-account.png): 1280 × 800 viewport, full-page capture.
- [Mobile account settings](mobile-account.png): 390 × 844 touch-emulated viewport, full-page capture.
- [Sanitized machine-readable results](browser-evidence.json): ten desktop/mobile states, browser/tool versions, image hashes and assertions.

The Chrome/Playwright check exercised protected first-owner setup, real account creation/revocation with typed confirmation, password change followed by actual reload/sign-out, old-password/session rejection, TOTP enrollment/code-required login/disable, and mobile menu focus containment/background inertness/Escape focus restoration. Captured states had zero uncaught page errors, axe WCAG A/AA findings or horizontal viewport overflow. The shown fields contain no passwords, enrollment secrets or session tokens.

This is bounded browser evidence, not a production-readiness badge. It does not prove real Minecraft/systemd operation, per-instance browser workflows, HTTPS proxy deployment, physical-device/screen-reader compatibility, or complete accessibility. See [acceptance](../../acceptance.md) for remaining requirements.

## Update-policy regression confirmation

At `255dcc7` on **2026-11-02**, a new disposable embedded panel passed the real UI/API postponement flow: saving ordinary policy fields retained the deadline, then background refresh reflected explicit replacement and clearing from another authenticated request. [Results and hashes](update-policy-evidence.json), [desktop](desktop-updates.png) and [mobile](mobile-updates.png) record the final confirmation. Both views had no horizontal overflow, uncaught page errors or captured axe A/AA findings. The panel stopped gracefully afterward.

The shown `dev` build correctly reports that release updating is unavailable for a non-SemVer installation. It is not a staged or verified official release, and this check does not exercise the Linux privileged updater.
