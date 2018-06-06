// Copyright 2017 Pilosa Corp.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pilosa

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/pilosa/pilosa/internal"
	"github.com/pkg/errors"
)

// Index represents a container for frames.
type Index struct {
	mu   sync.RWMutex
	path string
	name string

	// Fields by name.
	fields map[string]*Field

	// Max Slice on any node in the cluster, according to this node.
	remoteMaxSlice uint64

	NewAttrStore func(string) AttrStore

	// Column attribute storage and cache.
	columnAttrStore AttrStore

	broadcaster Broadcaster
	Stats       StatsClient

	Logger Logger
}

// NewIndex returns a new instance of Index.
func NewIndex(path, name string) (*Index, error) {
	err := ValidateName(name)
	if err != nil {
		return nil, errors.Wrap(err, "validating name")
	}

	return &Index{
		path:   path,
		name:   name,
		fields: make(map[string]*Field),

		remoteMaxSlice: 0,

		NewAttrStore:    NewNopAttrStore,
		columnAttrStore: NopAttrStore,

		broadcaster: NopBroadcaster,
		Stats:       NopStatsClient,
		Logger:      NopLogger,
	}, nil
}

// Name returns name of the index.
func (i *Index) Name() string { return i.name }

// Path returns the path the index was initialized with.
func (i *Index) Path() string { return i.path }

// ColumnAttrStore returns the storage for column attributes.
func (i *Index) ColumnAttrStore() AttrStore { return i.columnAttrStore }

// Options returns all options for this index.
func (i *Index) Options() IndexOptions {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.options()
}

func (i *Index) options() IndexOptions {
	return IndexOptions{}
}

// Open opens and initializes the index.
func (i *Index) Open() error {
	// Ensure the path exists.
	if err := os.MkdirAll(i.path, 0777); err != nil {
		return errors.Wrap(err, "creating directory")
	}

	// Read meta file.
	if err := i.loadMeta(); err != nil {
		return errors.Wrap(err, "loading meta file")
	}

	if err := i.openFields(); err != nil {
		return errors.Wrap(err, "opening frames")
	}

	if err := i.columnAttrStore.Open(); err != nil {
		return errors.Wrap(err, "opening attrstore")
	}

	return nil
}

// openFields opens and initializes the frames inside the index.
func (i *Index) openFields() error {
	f, err := os.Open(i.path)
	if err != nil {
		return errors.Wrap(err, "opening directory")
	}
	defer f.Close()

	fis, err := f.Readdir(0)
	if err != nil {
		return errors.Wrap(err, "reading directory")
	}

	for _, fi := range fis {
		if !fi.IsDir() {
			continue
		}

		fld, err := i.newField(i.FieldPath(filepath.Base(fi.Name())), filepath.Base(fi.Name()))
		if err != nil {
			return ErrName
		}
		if err := fld.Open(); err != nil {
			return fmt.Errorf("open frame: name=%s, err=%s", fld.Name(), err)
		}
		i.fields[fld.Name()] = fld
	}
	return nil
}

// loadMeta reads meta data for the index, if any.
func (i *Index) loadMeta() error {
	var pb internal.IndexMeta

	// Read data from meta file.
	buf, err := ioutil.ReadFile(filepath.Join(i.path, ".meta"))
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return errors.Wrap(err, "reading")
	} else {
		if err := proto.Unmarshal(buf, &pb); err != nil {
			return errors.Wrap(err, "unmarshalling")
		}
	}

	// Copy metadata fields.

	return nil
}

// NOTE: Until we introduce new attributes to store in the index .meta file,
// we don't need to actually write the file. The code related to index.options
// and the index meta file are left in place for future use.
/*
// saveMeta writes meta data for the index.
func (i *Index) saveMeta() error {
	// Marshal metadata.
	buf, err := proto.Marshal(&internal.IndexMeta{})
	if err != nil {
		return errors.Wrap(err, "marshalling")
	}

	// Write to meta file.
	if err := ioutil.WriteFile(filepath.Join(i.path, ".meta"), buf, 0666); err != nil {
		return errors.Wrap(err, "writing")
	}

	return nil
}
*/

// Close closes the index and its frames.
func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Close the attribute store.
	i.columnAttrStore.Close()

	// Close all frames.
	for _, f := range i.fields {
		if err := f.Close(); err != nil {
			return errors.Wrap(err, "closing frame")
		}
	}
	i.fields = make(map[string]*Field)

	return nil
}

// MaxSlice returns the max slice in the index according to this node.
func (i *Index) MaxSlice() uint64 {
	if i == nil {
		return 0
	}
	i.mu.RLock()
	defer i.mu.RUnlock()

	max := i.remoteMaxSlice
	for _, f := range i.fields {
		if slice := f.MaxSlice(); slice > max {
			max = slice
		}
	}

	i.Stats.Gauge("maxSlice", float64(max), 1.0)
	return max
}

// SetRemoteMaxSlice sets the remote max slice value received from another node.
func (i *Index) SetRemoteMaxSlice(newmax uint64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.remoteMaxSlice = newmax
}

// FieldPath returns the path to a field in the index.
func (i *Index) FieldPath(name string) string { return filepath.Join(i.path, name) }

// Field returns a frame in the index by name.
func (i *Index) Field(name string) *Field {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.field(name)
}

func (i *Index) field(name string) *Field { return i.fields[name] }

// Fields returns a list of all fields in the index.
func (i *Index) Fields() []*Field {
	i.mu.RLock()
	defer i.mu.RUnlock()

	a := make([]*Field, 0, len(i.fields))
	for _, f := range i.fields {
		a = append(a, f)
	}
	sort.Sort(fieldSlice(a))

	return a
}

// RecalculateCaches recalculates caches on every frame in the index.
func (i *Index) RecalculateCaches() {
	for _, frame := range i.Fields() {
		frame.RecalculateCaches()
	}
}

// CreateField creates a field.
func (i *Index) CreateField(name string, opt FieldOptions) (*Field, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Ensure frame doesn't already exist.
	if i.fields[name] != nil {
		return nil, ErrFieldExists
	}
	return i.createField(name, opt)
}

// CreateFieldIfNotExists creates a field with the given options if it doesn't exist.
func (i *Index) CreateFieldIfNotExists(name string, opt FieldOptions) (*Field, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Find frame in cache first.
	if f := i.fields[name]; f != nil {
		return f, nil
	}

	return i.createField(name, opt)
}

func (i *Index) createField(name string, opt FieldOptions) (*Field, error) {
	if name == "" {
		return nil, errors.New("frame name required")
	} else if opt.CacheType != "" && !IsValidCacheType(opt.CacheType) {
		return nil, ErrInvalidCacheType
	}

	// Validate options.
	if err := opt.Validate(); err != nil {
		return nil, errors.Wrap(err, "validating options")
	}

	// Initialize frame.
	f, err := i.newField(i.FieldPath(name), name)
	if err != nil {
		return nil, errors.Wrap(err, "initializing")
	}

	// Open frame.
	if err := f.Open(); err != nil {
		return nil, errors.Wrap(err, "opening")
	}

	// Apply frame options.
	if err := f.applyOptions(opt); err != nil {
		f.Close()
		return nil, errors.Wrap(err, "applying options")
	}

	if err := f.saveMeta(); err != nil {
		f.Close()
		return nil, errors.Wrap(err, "saving meta")
	}

	// Add to index's frame lookup.
	i.fields[name] = f

	return f, nil
}

func (i *Index) newField(path, name string) (*Field, error) {
	f, err := NewField(path, i.name, name)
	if err != nil {
		return nil, err
	}
	f.Logger = i.Logger
	f.Stats = i.Stats.WithTags(fmt.Sprintf("frame:%s", name))
	f.broadcaster = i.broadcaster
	f.rowAttrStore = i.NewAttrStore(filepath.Join(f.path, ".data"))
	return f, nil
}

// DeleteField removes a field from the index.
func (i *Index) DeleteField(name string) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Ignore if frame doesn't exist.
	f := i.field(name)
	if f == nil {
		return nil
	}

	// Close frame.
	if err := f.Close(); err != nil {
		return errors.Wrap(err, "closing")
	}

	// Delete frame directory.
	if err := os.RemoveAll(i.FieldPath(name)); err != nil {
		return errors.Wrap(err, "removing directory")
	}

	// Remove reference.
	delete(i.fields, name)

	return nil
}

type indexSlice []*Index

func (p indexSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p indexSlice) Len() int           { return len(p) }
func (p indexSlice) Less(i, j int) bool { return p[i].Name() < p[j].Name() }

// IndexInfo represents schema information for an index.
type IndexInfo struct {
	Name   string       `json:"name"`
	Fields []*FieldInfo `json:"fields"`
}

type indexInfoSlice []*IndexInfo

func (p indexInfoSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p indexInfoSlice) Len() int           { return len(p) }
func (p indexInfoSlice) Less(i, j int) bool { return p[i].Name < p[j].Name }

// EncodeIndexes converts a into its internal representation.
func EncodeIndexes(a []*Index) []*internal.Index {
	other := make([]*internal.Index, len(a))
	for i := range a {
		other[i] = encodeIndex(a[i])
	}
	return other
}

// encodeIndex converts d into its internal representation.
func encodeIndex(d *Index) *internal.Index {
	return &internal.Index{
		Name:   d.name,
		Fields: encodeFields(d.Fields()),
	}
}

// IndexOptions represents options to set when initializing an index.
type IndexOptions struct{}

// Encode converts i into its internal representation.
func (i *IndexOptions) Encode() *internal.IndexMeta {
	return &internal.IndexMeta{}
}

// hasTime returns true if a contains a non-nil time.
func hasTime(a []*time.Time) bool {
	for _, t := range a {
		if t != nil {
			return true
		}
	}
	return false
}

type importKey struct {
	View  string
	Slice uint64
}

type importData struct {
	RowIDs    []uint64
	ColumnIDs []uint64
}

type importValueData struct {
	ColumnIDs []uint64
	Values    []int64
}
