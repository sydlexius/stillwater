# Hero fixture - public-domain image provenance (#1756)

Every portrait in the cinematic README hero fixture is sourced from Wikimedia
Commons and verified free-to-redistribute from the file's own Commons license
metadata at fetch time - NOT inferred from the composer's death date (a modern
photo, museum scan, or statue image of a dead composer can carry its own
copyright). Regenerate with `docs/hero/enrich-fixture.sh`, which re-checks each
license and rejects anything outside the free-license allowlist.

Auto-accept (LicenseShortName, case-insensitive substring): `public domain`,
`PD-*`, `CC0`, `CC BY`, `CC BY-SA`, `no restrictions`. Hard-rejected even if a
permissive token also appears: NonCommercial, NoDerivs, fair-use, all-rights-
reserved. Attribution is captured below for CC-BY(-SA) images.

| Composer | Image (slot) | License | Commons source |
|---|---|---|---|
| Johann Sebastian Bach | folder.jpg (thumb) | Public domain (by Elias Gottlob Haussmann) | https://commons.wikimedia.org/wiki/File:Johann_Sebastian_Bach.jpg |
| Wolfgang Amadeus Mozart | folder.jpg (thumb) | Public domain (by Johann Nepomuk della Croce) | https://commons.wikimedia.org/wiki/File:The_Mozart_Family_-_Wolfgang_Amadeus_Mozart_headshot.jpg |
| Ludwig van Beethoven | folder.jpg (thumb) | Public domain (by Joseph Karl Stieler) | https://commons.wikimedia.org/wiki/File:Joseph_Karl_Stieler's_Beethoven_mit_dem_Manuskript_der_Missa_solemnis.jpg |
| Antonio Vivaldi | folder.jpg (thumb) | Public domain (by Unidentified painter) | https://commons.wikimedia.org/wiki/File:Vivaldi.jpg |
| George Frideric Handel | folder.jpg (thumb) | Public domain (by Attributed to Balthasar Denner) | https://commons.wikimedia.org/wiki/File:George_Frideric_Handel_by_Balthasar_Denner.jpg |
| Johannes Brahms | folder.jpg (thumb) | Public domain (by C. Brasch, Berlin (biography)) | https://commons.wikimedia.org/wiki/File:JohannesBrahms_(cropped).jpg |
| Claude Debussy | folder.jpg (thumb) | Public domain (by Adam Cuerden) | https://commons.wikimedia.org/wiki/File:Claude_Debussy_by_Atelier_Nadar.jpg |
| Frederic Chopin | folder.jpg (thumb) | Public domain (by Louis-Auguste Bisson) | https://commons.wikimedia.org/wiki/File:Frederic_Chopin_photo.jpeg |
| Pyotr Ilyich Tchaikovsky | folder.jpg (thumb) | Public domain (by Émile Reutlinger) | https://commons.wikimedia.org/wiki/File:Tchaikovsky_by_Reutlinger_(cropped).jpg |
| Franz Schubert | folder.jpg (thumb) | Public domain (by Wilhelm August Rieder) | https://commons.wikimedia.org/wiki/File:Franz_Schubert_by_Wilhelm_August_Rieder_1875.jpg |
| Joseph Haydn | folder.jpg (thumb) | Public domain (by Thomas Hardy) | https://commons.wikimedia.org/wiki/File:Joseph_Haydn.jpg |
| Franz Liszt | folder.jpg (thumb) | Public domain (by Herman Biow) | https://commons.wikimedia.org/wiki/File:Franz_Liszt_by_Herman_Biow-_1843.png |
