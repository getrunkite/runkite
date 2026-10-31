# Bundled fonts

These WOFF2 assets retain their upstream SIL Open Font License 1.1, separately
from Runkite's Apache-2.0 license:

- Inter 4.001: Copyright 2016 The Inter Project Authors. See
  [Inter-OFL.txt](../../../public/licenses/Inter-OFL.txt) and the
  [upstream license](https://github.com/rsms/inter/blob/master/LICENSE.txt).
- JetBrains Mono 2.211: Copyright 2020 The JetBrains Mono Project Authors. See
  [JetBrainsMono-OFL.txt](../../../public/licenses/JetBrainsMono-OFL.txt) and the
  [upstream license](https://github.com/JetBrains/JetBrainsMono/blob/master/OFL.txt).

The versions and copyright statements above match the font name metadata.
The subset files cover Latin and Latin Extended characters; see `../../fonts.css`.

Vite copies `admin-ui/public/licenses/` into the embedded Admin bundle. Keep those
license texts with the fonts when rebuilding or distributing that bundle. The Go
plane serves them at `/admin/licenses/Inter-OFL.txt` and
`/admin/licenses/JetBrainsMono-OFL.txt`.
