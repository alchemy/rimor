package export

import (
	"os"
	"strings"
)

// ListSeparator is the CSV separator the system's locale expects: ';'
// where numbers are written with a decimal comma (Excel reads CSV with
// ';' there, as in Italian, German or French), else ','. On Windows it is
// the user's list separator setting itself.
func ListSeparator() rune {
	if r := systemListSeparator(); r != 0 {
		return r
	}
	return separatorFor(localeName())
}

// DecimalSeparator is how the system's locale writes decimals: ',' or
// '.'. On Windows it is the region setting itself.
func DecimalSeparator() rune {
	if r := systemDecimalSeparator(); r != 0 {
		return r
	}
	if commaLocale(localeName()) {
		return ','
	}
	return '.'
}

// localeName is the locale for numbers, from the environment; "" for the
// C locale or none.
func localeName() string {
	for _, v := range []string{"LC_ALL", "LC_NUMERIC", "LANG"} {
		if loc := os.Getenv(v); loc != "" {
			if loc == "C" || loc == "POSIX" || strings.HasPrefix(loc, "C.") {
				return ""
			}
			return loc
		}
	}
	return ""
}

// separatorFor is the CSV separator for a POSIX locale name.
func separatorFor(loc string) rune {
	if commaLocale(loc) {
		return ';'
	}
	return ','
}

// commaLocale reports whether a POSIX locale name,
// language_REGION.encoding@variant, writes decimals with a comma.
func commaLocale(loc string) bool {
	loc, _, _ = strings.Cut(loc, ".")
	loc, _, _ = strings.Cut(loc, "@")
	lang, region, _ := strings.Cut(strings.ReplaceAll(loc, "-", "_"), "_")
	lang, region = strings.ToLower(lang), strings.ToUpper(region)
	if comma, ok := regionDecimal[lang+"_"+region]; ok {
		return comma
	}
	return decimalComma[lang]
}

// decimalComma are languages whose numbers usually have a decimal comma.
var decimalComma = func() map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Fields(`af az be bg bs ca cs da de el es et eu fi fo fr gl hr hu hy id is it
		ka kk ky lt lv mk mn nb nl nn no pl pt ro ru sk sl sq sr sv tr uk uz vi`) {
		m[l] = true
	}
	return m
}()

// regionDecimal are the regions that differ from their language: true for
// a decimal comma.
var regionDecimal = map[string]bool{
	"de_CH": false, "de_LI": false, "it_CH": false,
	"es_MX": false, "es_US": false, "es_PR": false, "es_GT": false, "es_HN": false,
	"es_NI": false, "es_PA": false, "es_SV": false, "es_DO": false,
	"en_ZA": true,
}
