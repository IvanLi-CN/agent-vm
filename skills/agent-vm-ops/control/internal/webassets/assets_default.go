//go:build !releaseassets

package webassets

import (
	"errors"
	"io/fs"
)

var Files fs.FS

func Verify() error {
	return errors.New("web_assets_missing: control binary was not built with release assets")
}
