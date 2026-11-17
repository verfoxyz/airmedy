package domain

import (
	"reflect"
	"testing"
)

func TestNormalizeCatalogSourceOrderDefaultsToAlphabetical(t *testing.T) {
	got := NormalizeCatalogSourceOrder(nil, []string{"wy", "kg", "tx"})
	want := []string{"kg", "tx", "wy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestNormalizeCatalogSourceOrderKeepsStoredOrder(t *testing.T) {
	got := NormalizeCatalogSourceOrder(
		[]string{"tx", "wy"}, []string{"kg", "kw", "mg", "tx", "wy"})
	want := []string{"tx", "wy", "kg", "kw", "mg"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestNormalizeCatalogSourceOrderDropsUnknownAndDuplicates(t *testing.T) {
	got := NormalizeCatalogSourceOrder(
		[]string{" KG ", "unknown", "kg", "", "wy"}, []string{"kg", "wy"})
	want := []string{"kg", "wy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestCatalogSourceOrderRoundTrip(t *testing.T) {
	encoded, err := EncodeCatalogSourceOrder([]string{"wy", "kg"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !reflect.DeepEqual(DecodeCatalogSourceOrder(encoded), []string{"wy", "kg"}) {
		t.Fatalf("decoded = %v", DecodeCatalogSourceOrder(encoded))
	}
	if got := DecodeCatalogSourceOrder("not json"); got != nil {
		t.Fatalf("malformed value = %v, want nil", got)
	}
}

func TestNormalizeDisabledSourcesDropsUnknownAndDuplicates(t *testing.T) {
	got := NormalizeDisabledSources(
		[]string{" KG ", "unknown", "kg", "wy"}, []string{"kg", "kw", "wy"})
	want := []string{"kg", "wy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("disabled = %v, want %v", got, want)
	}
	if got := NormalizeDisabledSources(nil, []string{"kg"}); len(got) != 0 {
		t.Fatalf("empty set = %v, want none", got)
	}
}
