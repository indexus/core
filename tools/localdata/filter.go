package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// exportFilter selects the records of one experience from a larger database
// and rewrites the native locations written with the former alphabet.
type exportFilter struct {
	collections []string // whole collections: exact names, or prefixes ending in '*'
	paths       []string // content parts whose path starts with one of these, in any collection
	translate   []string // native collections to translate, same syntax as collections
	within      []string // COLLECTION:PREFIX, native records of COLLECTION kept only under PREFIX (after translation)
}

func splitList(raw string) []string {
	var values []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func matches(patterns []string, collection string) bool {
	for _, pattern := range patterns {
		if prefix, wildcard := strings.CutSuffix(pattern, "*"); wildcard && strings.HasPrefix(collection, prefix) || pattern == collection {
			return true
		}
	}
	return false
}

// contentPath reports the path of a content part (the envelope written by
// -assets and by experiences' publish-content-assets.mjs).
func contentPath(r storage.Record) (string, bool) {
	if len(r.Payload) == 0 || r.Payload[0] != '{' {
		return "", false
	}
	var part struct {
		Path  string `json:"path"`
		Parts int    `json:"parts"`
	}
	if json.Unmarshal(r.Payload, &part) != nil || part.Path == "" || part.Parts < 1 {
		return "", false
	}
	return part.Path, true
}

// keep reports whether r, a content part at path or a native record, belongs
// to the export. Tombstones are dropped: the export seeds a fresh network,
// where an absent key is already deleted.
func (f exportFilter) keep(r storage.Record, path string, content bool) bool {
	if r.Deleted {
		return false
	}
	if matches(f.collections, r.Collection) {
		return true
	}
	if !content {
		return false
	}
	for _, prefix := range f.paths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// inside reports whether r, after translation, lies under the location
// prefix given for its collection: a small area of a large native collection.
func (f exportFilter) inside(r storage.Record, content bool) bool {
	if content {
		return true
	}
	for _, rule := range f.within {
		collection, prefix, _ := strings.Cut(rule, ":")
		if matches([]string{collection}, r.Collection) && !strings.HasPrefix(r.Location, prefix) {
			return false
		}
	}
	return true
}

// apply translates r in place when its collection is named and it is not a
// content part: content is addressed by a hash of its path, not by a cell.
func (f exportFilter) apply(r *storage.Record, content bool) (bool, error) {
	if content || !matches(f.translate, r.Collection) {
		return false, nil
	}
	location, err := translateLocation(r.Collection, r.Location)
	if err != nil {
		return false, err
	}
	r.Location = location
	return true, nil
}

// oldAlphabet is the former geometry alphabet of the SDK's Space. It survives
// only here and in sdk/scripts/migrate/translate-locations.mjs, to read old
// data (sdk/docs/alphabet-migration.md).
const oldAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// translateLocation gives back the cell the old client decoded from each
// character and names it with the single alphabet. Past the collection name
// the old client decoded every character as cell 0, and so does this.
func translateLocation(collection, location string) (string, error) {
	if location == tree.BASE64.Root() || location == "" {
		return location, nil
	}
	if collection == "" {
		return "", fmt.Errorf("translate: empty collection name")
	}
	routing := tree.BASE64
	out := make([]byte, 0, len(location))
	for l := range len(location) {
		index := strings.IndexByte(oldAlphabet, location[l])
		if index < 0 {
			return "", fmt.Errorf("translate: %q in %s/%s is not a location character", location[l], collection, location)
		}
		cell := 0
		if l < len(collection) {
			cell = (index - strings.IndexByte(oldAlphabet, collection[l]) + 64) % 64
		}
		offset := routing.IndexOf(rune(collection[l%len(collection)]))
		if offset < 0 {
			return "", fmt.Errorf("translate: collection %q is not in the location alphabet", collection)
		}
		out = append(out, routing.CharAt((cell+offset)%64)...)
	}
	return string(out), nil
}
