// Repository hygiene guards. They have their own go.mod so they are not
// part of the published github.com/helix-tools/sdk-go/v2 module zip (the zip
// excludes every subdirectory that has its own go.mod); CI runs them as a
// separate step.
module github.com/helix-tools/sdk-go/v2/internal/reposafety

go 1.25.12
