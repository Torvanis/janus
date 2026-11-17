package config

import (
	"strings"
	"testing"
)

func TestPriceCatalogEnvironment(t *testing.T) {
	setBaseline(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PriceCatalogRefresh != nil || c.PriceCatalogURL != "" || c.PriceCatalogSignatureOff || len(c.PriceCatalogKeys) != 0 {
		t.Fatalf("defaults: refresh=%v url=%q signatureOff=%v keys=%v", c.PriceCatalogRefresh, c.PriceCatalogURL, c.PriceCatalogSignatureOff, c.PriceCatalogKeys)
	}
	t.Setenv("JANUS_PRICE_CATALOG_REFRESH", "daily")
	t.Setenv("JANUS_PRICE_CATALOG_URL", "https://prices.internal.example/catalog.json")
	t.Setenv("JANUS_PRICE_CATALOG_REQUIRE_SIGNATURE", "false")
	t.Setenv("JANUS_PRICE_CATALOG_KEYS", "FU/XAvq0WqnoJRmYUtn63XQaEBsbeXGaZ2HZAFCJQUI=, G63VMTaZ0tBqyNCVJKOXbnMRtbJOrSitb0yLE9Wpq9I=")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if c.PriceCatalogRefresh == nil || !*c.PriceCatalogRefresh || c.PriceCatalogURL != "https://prices.internal.example/catalog.json" || !c.PriceCatalogSignatureOff || len(c.PriceCatalogKeys) != 2 {
		t.Fatalf("set: %+v", c)
	}
	t.Setenv("JANUS_PRICE_CATALOG_REFRESH", "off")
	if c, err = Load(); err != nil || c.PriceCatalogRefresh == nil || *c.PriceCatalogRefresh {
		t.Fatalf("explicit off must pin: %v", err)
	}
	for name, bad := range map[string][2]string{
		"refresh": {"JANUS_PRICE_CATALOG_REFRESH", "hourly"},
		"url":     {"JANUS_PRICE_CATALOG_URL", "ftp://x.example/c.json"},
		"creds":   {"JANUS_PRICE_CATALOG_URL", "https://u:p@x.example/c.json"},
		"keys":    {"JANUS_PRICE_CATALOG_KEYS", "c2hvcnQ="},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(bad[0], bad[1])
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), bad[0]) {
				t.Fatalf("%s=%s accepted: %v", bad[0], bad[1], err)
			}
		})
	}
}
