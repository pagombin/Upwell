# Vendored: github.com/go-pdf/fpdf v0.9.0

Copied from the module cache (`go get github.com/go-pdf/fpdf@v0.9.0`) for the
PDF report (decision D71), so the build needs no network for it. Only the core
package is kept: its `.go` files (tests removed), the embedded core-font
metrics in `font_embed/`, the LICENSE (MIT) and the README. The `contrib`,
`font` (TrueType files), `image`, `pdf`, `doc`, `makefont` and `internal`
directories are left out; the core package imports only the standard library.
`go.mod` here declares no requirements (the upstream ones are for `contrib`).
The root `go.mod` pins v0.9.0 and replaces it with this directory.
