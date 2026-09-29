---
paths: ["app/**"]
---
- Bare React Native, no Expo: no Expo modules, no EAS, no Expo account in the
  release path. `app/ios/` is committed and is the source of truth for native
  config; `npm run check:ios` asserts the parts that are promises to the user.
- TypeScript strict; npm run typecheck must pass.
- No inventory state persisted in the app beyond the offline capture queue;
  Homebox via the backend is the system of record.
- Copy style: sentence case, active verbs, CTAs say what happens
  ("Take photo", "Find a box", "Catalog another item").
- A new native module needs: its iOS privacy-manifest implications checked
  (does it ship a PrivacyInfo.xcprivacy? which required-reason APIs does its
  binary touch?), a jest fake in test/ if it loads native code at import, and
  `cd ios && bundle exec pod install` with Podfile.lock committed.
- Never add a permission the app does not ask for at runtime.
