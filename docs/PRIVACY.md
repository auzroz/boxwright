# Privacy policy

Boxwright is a self-hosted companion app for [Homebox](https://github.com/sysadminsmedia/homebox).
This policy covers the Boxwright iOS app and the Boxwright server software.

## What the developers receive

Nothing. The app contains no analytics, advertising, crash-reporting or
tracking code, and it never contacts a server operated by the Boxwright
project.

## Where your data goes

The app sends data only to the Boxwright server whose address you enter in its
settings, which you or someone you trust runs. That server stores what you
catalog in your own Homebox. Specifically:

- **Photos** you take or choose are reduced to 1024 pixels and stripped of
  their metadata (including any location), then sent to your Boxwright server
  to identify the item and attached to the item in your Homebox when you file
  it. If a photo cannot be reduced, the original is sent as it is rather than
  losing the capture, and keeps whatever metadata it had. Choosing a photo uses
  the system picker, so the app never has access to the rest of your library.
  The clipboard is read only when you tap "Paste a copied image", never in the
  background, and a pasted image is handled exactly like a photo.
- **Item details** (name, category, size, notes) are sent to your Boxwright
  server and stored in your Homebox. On an iPhone with LiDAR, that includes
  the measurements the app takes from depth (an item's size in centimetres, or
  how full a container is as a percentage) -- the numbers only, never the
  depth itself.
- **Server address and tokens** you enter are stored on your device in the iOS
  Keychain and sent only to the servers they belong to.
- **Your Homebox theme's name** (for example "forest") is read from your own
  Homebox by your Boxwright server, so the app can use its colour. It is kept
  on the phone with your other preferences and sent nowhere else.

If your Boxwright server is configured to use a cloud vision model (for
example OpenAI or Anthropic), it sends each photo to that provider for
identification, under that provider's terms. This is a setting on the server
you run; with `AI_PROVIDER=none` or a local model, photos never leave your own
hardware.

## What stays on your device

Photos waiting to be identified or reviewed, and captures waiting for signal (a
photo and its item details), are kept in the app's private storage until they
reach your server or you discard them, then deleted.

**Depth maps** never leave the phone. On an iPhone with LiDAR, the in-app
camera uses Apple's ARKit, which runs entirely on the device, and saves each
photo's depth map (about 145 KB: distances, the camera's position and angle at
that moment relative to where the camera screen opened, and the flat surfaces
ARKit found) beside the photo in the app's documents
directory. The app measures from it on the phone. It is deleted as soon as the
capture has been reviewed -- filed, queued or discarded -- and is never
uploaded, not even to your own server: only the centimetre and percentage
results travel, as part of the item details above. The in-app camera's photo
is written without any metadata, so it carries no location. The motion sensors
ARKit uses to know which way is down are read by ARKit itself; nothing from
them is kept beyond the camera pose of that one photo.

A copy of your box list and category list is cached so the app can work
offline.
Deleting the app deletes all of it except the saved server settings: iOS can
keep Keychain entries after an app is removed. They are replaced the next time
settings are saved.

## Contact

Open an issue at <https://github.com/auzroz/boxwright/issues>.
