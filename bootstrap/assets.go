package bootstrap

import (
	"fmt"
	"gnd.la/template/assets"
	"gnd.la/util/semver"
)

const (
	bootstrapCSSFmt              = "//netdna.bootstrapcdn.com/bootstrap/%s/css/bootstrap.min.css"
	bootstrapCSSNoIconsFmt       = "//netdna.bootstrapcdn.com/bootstrap/%s/css/bootstrap.no-icons.min.css"
	bootstrapCSSNoIconsLegacyFmt = "//netdna.bootstrapcdn.com/bootstrap/%s/css/bootstrap-combined.no-icons.min.css"
	bootstrapJSFmt               = "//netdna.bootstrapcdn.com/bootstrap/%s/js/bootstrap.min.js"
	fontAwesomeFmt               = "//netdna.bootstrapcdn.com/font-awesome/%s/css/font-awesome.min.css"
)

func bootstrapParser(m assets.Manager, names []string, options assets.Options) ([]assets.Asset, error) {
	if len(names) > 1 {
		return nil, fmt.Errorf("invalid bootstrap declaration \"%s\": must include only a version number", names)
	}
	bsV := names[0]
	bsVersion, err := semver.Parse(bsV)
	if err != nil || bsVersion.PreRelease != "" || bsVersion.Build != "" {
		return nil, fmt.Errorf("invalid bootstrap version %q", bsV)
	}
	if bsVersion.Major != 2 && bsVersion.Major != 3 {
		return nil, fmt.Errorf("only bootstrap versions 2.x and 3.x are supported")
	}
	var as []assets.Asset
	if options.BoolOpt("fontawesome", m) {
		faV := options.StringOpt("fontawesome", m)
		if faV == "" {
			return nil, fmt.Errorf("please, specify a font awesome version")
		}
		faVersion, err := semver.Parse(faV)
		if err != nil || faVersion.PreRelease != "" || faVersion.Build != "" {
			return nil, fmt.Errorf("invalid font awesome version %q", faV)
		}
		if faVersion.Major != 3 && faVersion.Major != 4 {
			return nil, fmt.Errorf("only font awesome versions 3.x and 4.x are supported")
		}
		format := bootstrapCSSFmt
		if bsVersion.Major == 2 {
			format = bootstrapCSSNoIconsLegacyFmt
		} else if faVersion.Major == 3 {
			if bsVersion.Major >= 3 && (bsVersion.Minor > 0 || bsVersion.Patch > 0) {
				return nil, fmt.Errorf("can't use bootstrap > 3.0.0 with font awesome 3 (bootstrapcdn does not provide the files)")
			} else {
				format = bootstrapCSSNoIconsFmt
			}
		}
		as = append(as, &assets.Css{
			Common: assets.Common{
				Manager: m,
				Name:    fmt.Sprintf("bootstrap-noicons-%s.css", bsV),
			},
			Href: fmt.Sprintf(format, bsV),
		})
		as = append(as, &assets.Css{
			Common: assets.Common{
				Manager: m,
				Name:    fmt.Sprintf("fontawesome-%s.css", faV),
			},
			Href: fmt.Sprintf(fontAwesomeFmt, faV),
		})
	} else {
		as = append(as, &assets.Css{
			Common: assets.Common{
				Manager: m,
				Name:    fmt.Sprintf("bootstrap-%s.css", bsV),
			},
			Href: fmt.Sprintf(bootstrapCSSFmt, bsV),
		})
	}
	if !options.BoolOpt("nojs", m) {
		as = append(as, &assets.Script{
			Common: assets.Common{
				Manager: m,
				Name:    fmt.Sprintf("bootstrap-%s.js", bsV),
			},
			Src: fmt.Sprintf(bootstrapJSFmt, bsV),
		})
	}
	return as, nil
}

func init() {
	assets.Register("bootstrap", bootstrapParser)
}
