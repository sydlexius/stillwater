# M4: Image Management (v0.4.0)

## Goal

Build the image search, comparison, upload, crop, and processing pipeline. Users can find, compare, and save artist images from providers or manual uploads.

## Prerequisites

- M2 complete (artist model, scanner with image detection)
- M3 complete (provider adapters return ImageResult)

## Issues

| # | Title | Mode | Model |
|---|-------|------|-------|
| 12 | Image search UI with ranked grid and source badges | plan | opus |
| 13 | Image comparison panel (side-by-side) | direct | sonnet |
| 14 | Image upload: URL input and file upload | direct | sonnet |
| 15 | Client-side image cropping with Cropper.js | direct | sonnet |
| 16 | Server-side image resize and optimization | direct | sonnet |
| 17 | Image naming convention settings | direct | sonnet |

## Implementation Order

### Step 1: Server-Side Image Processing (#16)

**Package:** `internal/image/`

Start with the backend processing before building UI.

1. `internal/image/processor.go`:
   - `Resize(src io.Reader, width, height int) (io.Reader, error)`
   - `Optimize(src io.Reader, format string, quality int) (io.Reader, error)`
   - `DetectFormat(src io.Reader) (string, error)`
   - `GetDimensions(src io.Reader) (width, height int, error)`
   - `ValidateAspectRatio(width, height int, expected float64, tolerance float64) bool`

2. Use pure Go libraries (no CGO):
   - `image/jpeg`, `image/png` from stdlib
   - `golang.org/x/image/draw` for high-quality resizing
   - `golang.org/x/image/webp` for WebP decoding

3. Tests with sample images of various formats and sizes

### Step 2: Image Naming Conventions (#17)

**Settings and filesystem logic:**

1. Add naming config to settings model:
   - Per image type: list of enabled filenames
   - Default: folder.jpg (thumb), fanart.jpg (fanart), logo.png (logo), banner.jpg (banner)

2. `internal/image/naming.go`:
   - `FileNames(imageType, settings) []string` -- returns all enabled names for a type
   - `Save(dir string, imageType string, data io.Reader, settings) error` -- writes to all enabled names

3. Settings UI section for image naming

### Step 3: Image Upload (#14)

**API endpoints and handlers:**

1. `POST /api/v1/artists/{id}/images/upload` -- multipart file upload
   - Validate file type (JPEG, PNG, WebP)
   - Validate file size (configurable max, default 25MB)
   - Process through image processor
   - Save to artist directory with configured naming

2. `POST /api/v1/artists/{id}/images/fetch` -- fetch from URL
   - Accept JSON body: `{"url": "...", "type": "thumb"}`
   - Fetch with timeout (30s) and size limit
   - Validate content type
   - Process and save

3. Upload UI component:
   - File input with drag-and-drop zone
   - URL paste input
   - Image type selector (thumb, fanart, logo, banner)
   - Progress indicator

### Step 4: Image Search UI (#12)

**Templates and HTMX interactions:**

1. `web/templates/image_search.templ`:
   - Search triggers provider orchestrator for all configured sources
   - Results displayed in ranked grid
   - Each card shows: thumbnail preview, source badge, resolution, file size
   - Warning badges: low resolution, wrong aspect ratio, transparency issues
   - Click card to show full-size preview modal

2. `web/components/image_card.templ`:
   - Reusable card component with source badge coloring
   - Warning badge icons with tooltips

3. API endpoint:
   - `GET /api/v1/search/images?artistId=...&type=thumb`
   - Returns aggregated results from all providers, sorted by priority

4. HTMX interactions:
   - Search triggers: artist ID + image type
   - Lazy-load thumbnails
   - Click to preview: swap modal content
   - "Save this image" button per card

### Step 5: Image Comparison Panel (#13)

**Side-by-side comparison component:**

1. `web/components/image_compare.templ`:
   - Two-panel layout (stacked on mobile)
   - Each panel shows: full image, metadata (source, resolution, size, aspect ratio)
   - "Use this one" button on each panel
   - Swap button to replace one panel with another candidate

2. HTMX interactions:
   - Select first image from grid (fills left panel)
   - Select second image from grid (fills right panel)
   - "Use this one" triggers save flow

### Step 6: Client-Side Cropping (#15)

**Cropper.js integration:**

1. Vendor Cropper.js in `web/static/js/cropper.min.js` + CSS
2. Crop modal component:
   - Opens after selecting/uploading an image
   - Aspect ratio presets: 1:1 (thumb), 16:9 (fanart), 5.4:1 (banner), free (logo)
   - Preview of cropped result
   - "Save cropped" sends crop data to server

3. Server endpoint for cropped upload:
   - Accept base64 or blob data from crop
   - Apply server-side processing (resize, optimize)
   - Save with configured naming

## Key Design Decisions

- **Pure Go image processing:** Using stdlib + golang.org/x/image to avoid CGO dependencies. Quality is sufficient for the resize/crop operations needed.
- **Progressive enhancement:** Image search works without JavaScript (basic form submit). Cropper.js enhances the experience but is not required for basic upload.
- **Lazy thumbnail loading:** Image grid uses HTMX lazy loading to avoid fetching all provider images at once.
- **File size limits:** Default 25MB max upload. Configurable in settings. URL fetch has separate timeout and size limit.

## Verification

- [ ] Image search returns results from all configured providers
- [ ] Source badges and warning badges display correctly
- [ ] Comparison panel works on desktop and mobile
- [ ] File upload and URL fetch both work
- [ ] Cropper.js modal opens with correct aspect ratio presets
- [ ] Server-side resize produces correct output dimensions
- [ ] Images saved with all enabled naming conventions
- [ ] `make test` and `make lint` pass
- [ ] Bruno collection updated
