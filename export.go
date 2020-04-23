/*
 * This file is subject to the terms and conditions defined in
 * file 'LICENSE.md', which is part of this source code package.
 */

package unitype

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/sirupsen/logrus"
)

// Font wraps font for outside access.
type Font struct {
	br *byteReader
	*font
}

// Parse parses the truetype font from `rs` and returns a new Font.
func Parse(rs io.ReadSeeker) (*Font, error) {
	r := newByteReader(rs)

	fnt, err := parseFont(r)
	if err != nil {
		return nil, err
	}

	return &Font{
		br:   r,
		font: fnt,
	}, nil
}

// ParseFile parses the truetype font from file given by path.
func ParseFile(filePath string) (*Font, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	defer f.Close()
	return Parse(f)
}

// ValidateBytes validates the turetype font represented by the byte stream.
func ValidateBytes(b []byte) error {
	r := bytes.NewReader(b)
	br := newByteReader(r)
	fnt, err := parseFont(br)
	if err != nil {
		return err
	}

	return fnt.validate(br)
}

// ValidateFile validates the truetype font given by `filePath`.
func ValidateFile(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	br := newByteReader(f)
	fnt, err := parseFont(br)
	if err != nil {
		return err
	}

	return fnt.validate(br)
}

// GetCmap returns the specific cmap specified by `platformID` and platform-specific `encodingID`.
// If not available, nil is returned. Used in PDF for decoding.
func (f *Font) GetCmap(platformID, encodingID int) map[rune]GlyphIndex {
	if f.cmap == nil {
		return nil
	}

	for _, subt := range f.cmap.subtables {
		if subt.platformID == platformID && subt.encodingID == encodingID {
			return subt.cmap
		}
	}

	return nil
}

// SubsetKeepRunes prunes data for all GIDs except the ones corresponding to `runes`.  The GIDs are
// maintained. Typically reduces glyf table size significantly.
func (f *Font) SubsetKeepRunes(runes []rune) (*Font, error) {
	var maps []map[rune]GlyphIndex
	// Search order (3,1), (1,0), (0,3).
	maps = append(maps, f.GetCmap(3, 1), f.GetCmap(1, 0), f.GetCmap(0, 3))

	var indices []GlyphIndex
	for _, r := range runes {
		index := GlyphIndex(0)
		for _, cmap := range maps {
			ind, has := cmap[r]
			if has {
				index = ind
				break
			}
		}
		if index == 0 {
			return nil, fmt.Errorf("rune not found: %v", r)
		}
		indices = append(indices, index)
	}
	logrus.Debugf("Runes: %+v %s", runes, string(runes))
	logrus.Debugf("GIDs: %+v", indices)
	return f.SubsetKeepIndices(indices)
}

// SubsetKeepIndices prunes data for all GIDs outside of `indices`. The GIDs are maintained.
// This typically works well and is a simple way to prune most of the unnecessary data as the
// glyf table is usually the biggest by far.
func (f *Font) SubsetKeepIndices(indices []GlyphIndex) (*Font, error) {
	newfnt := font{}

	gidIncludedMap := make(map[GlyphIndex]struct{}, len(indices))
	for _, gid := range indices {
		gidIncludedMap[gid] = struct{}{}
	}

	newfnt.ot = &offsetTable{}
	*newfnt.ot = *f.font.ot

	newfnt.trec = &tableRecords{}
	*newfnt.trec = *f.font.trec

	if f.font.head != nil {
		newfnt.head = &headTable{}
		*newfnt.head = *f.font.head
	}

	if f.font.maxp != nil {
		newfnt.maxp = &maxpTable{}
		*newfnt.maxp = *f.font.maxp
	}

	if f.font.hhea != nil {
		newfnt.hhea = &hheaTable{}
		*newfnt.hhea = *f.font.hhea
	}

	if f.font.hmtx != nil {
		newfnt.hmtx = &hmtxTable{}
		*newfnt.hmtx = *f.font.hmtx
		newfnt.optimizeHmtx()
	}

	if f.font.glyf != nil && f.font.loca != nil {
		newfnt.loca = &locaTable{}
		newfnt.glyf = &glyfTable{}
		*newfnt.glyf = *f.font.glyf

		// Empty glyf contents for non-included glyphs.
		for i := range newfnt.glyf.descs {
			if _, has := gidIncludedMap[GlyphIndex(i)]; has {
				continue
			}

			if newfnt.glyf.descs[i].IsSimple() {
				newfnt.glyf.descs[i].raw = nil
			} else {
				// TODO: For composite glyphs, need to know which ones are used together.
				//   If one gid relies on another on that is not included, need to include it.
				//   - Start by crawling through all the glyph descriptions and for any composite
				//     glyph in use, mark others that are required.
			}
		}

		// Update loca offsets.
		isShort := f.font.head.indexToLocFormat == 0
		if isShort {
			newfnt.loca.offsetsShort = make([]offset16, len(newfnt.glyf.descs)+1)
			newfnt.loca.offsetsShort[0] = f.font.loca.offsetsShort[0]
		} else {
			newfnt.loca.offsetsLong = make([]offset32, len(newfnt.glyf.descs)+1)
			newfnt.loca.offsetsLong[0] = f.font.loca.offsetsLong[0]
		}
		for i, desc := range newfnt.glyf.descs {
			if isShort {
				newfnt.loca.offsetsShort[i+1] = newfnt.loca.offsetsShort[i] + offset16(len(desc.raw))/2
			} else {
				newfnt.loca.offsetsLong[i+1] = newfnt.loca.offsetsLong[i] + offset32(len(desc.raw))
			}
		}
	}

	if f.font.prep != nil {
		newfnt.prep = &prepTable{}
		*newfnt.prep = *f.font.prep
	}

	if f.font.cvt != nil {
		newfnt.cvt = &cvtTable{}
		*newfnt.cvt = *f.font.cvt
	}

	if f.font.name != nil {
		newfnt.name = &nameTable{}
		*newfnt.name = *f.font.name
	}

	if f.font.os2 != nil {
		newfnt.os2 = &os2Table{}
		*newfnt.os2 = *f.font.os2
	}
	if f.font.post != nil {
		newfnt.post = &postTable{}
		*newfnt.post = *f.font.post
	}
	if f.font.cmap != nil {
		newfnt.cmap = &cmapTable{}
		*newfnt.cmap = *f.font.cmap
	}

	subfnt := &Font{
		br:   nil,
		font: &newfnt,
	}
	return subfnt, nil
}

// SubsetSimple creates a simple subset of `f` with only first `numGlyphs`.
// NOTE: Simple fonts are fonts limited to 0-255 character codes.
func (f *Font) SubsetSimple(numGlyphs int) (*Font, error) {
	if int(f.maxp.numGlyphs) <= numGlyphs {
		// TODO: Should just return the font back and log debug message?
		// User might not know the number of glyphs in the font apriori, unless we give some way to check.
		return nil, errors.New("no need to subset - already fewer or same amount of glyphs")
	}
	newfnt := font{}

	newfnt.ot = &offsetTable{}
	*newfnt.ot = *f.font.ot

	newfnt.trec = &tableRecords{}
	*newfnt.trec = *f.font.trec

	if f.font.head != nil {
		newfnt.head = &headTable{}
		*newfnt.head = *f.font.head
	}

	if f.font.maxp != nil {
		newfnt.maxp = &maxpTable{}
		*newfnt.maxp = *f.font.maxp
		newfnt.maxp.numGlyphs = uint16(numGlyphs)
	}
	if f.font.hhea != nil {
		newfnt.hhea = &hheaTable{}
		*newfnt.hhea = *f.font.hhea

		if newfnt.hhea.numberOfHMetrics > uint16(numGlyphs) {
			newfnt.hhea.numberOfHMetrics = uint16(numGlyphs)
		}
	}

	if f.font.hmtx != nil {
		newfnt.hmtx = &hmtxTable{}
		*newfnt.hmtx = *f.font.hmtx

		if len(newfnt.hmtx.hMetrics) > numGlyphs {
			newfnt.hmtx.hMetrics = newfnt.hmtx.hMetrics[0:numGlyphs]
			newfnt.hmtx.leftSideBearings = nil
		} else {
			numKeep := numGlyphs - len(newfnt.hmtx.hMetrics)
			if numKeep > len(newfnt.hmtx.leftSideBearings) {
				numKeep = len(newfnt.hmtx.leftSideBearings)
			}
			newfnt.hmtx.leftSideBearings = newfnt.hmtx.leftSideBearings[0:numKeep]
		}
		newfnt.optimizeHmtx()
	}

	if f.font.glyf != nil && f.font.loca != nil {
		newfnt.loca = &locaTable{}
		newfnt.glyf = &glyfTable{
			descs: f.font.glyf.descs[0:numGlyphs],
		}
		// Update loca offsets.
		isShort := f.font.head.indexToLocFormat == 0
		if isShort {
			newfnt.loca.offsetsShort = make([]offset16, numGlyphs+1)
			newfnt.loca.offsetsShort[0] = f.font.loca.offsetsShort[0]
		} else {
			newfnt.loca.offsetsLong = make([]offset32, numGlyphs+1)
			newfnt.loca.offsetsLong[0] = f.font.loca.offsetsLong[0]
		}
		for i, desc := range newfnt.glyf.descs {
			if !desc.IsSimple() {
				// TODO: Allow glyphs that are within the subset range: Can place the additional glyphs needed at the  end.
				// Only support simple glyphs here, since otherwise they could refer to outside the exported range.
				// Remove non-simple glyphs.
				desc.raw = nil
			}
			if isShort {
				newfnt.loca.offsetsShort[i+1] = newfnt.loca.offsetsShort[i] + offset16(len(desc.raw))/2
			} else {
				newfnt.loca.offsetsLong[i+1] = newfnt.loca.offsetsLong[i] + offset32(len(desc.raw))
			}
		}
	}

	if f.font.name != nil {
		newfnt.name = &nameTable{}
		*newfnt.name = *f.font.name
	}

	if f.font.os2 != nil {
		newfnt.os2 = &os2Table{}
		*newfnt.os2 = *f.font.os2
	}

	if f.font.post != nil {
		newfnt.post = &postTable{}
		*newfnt.post = *f.font.post

		if newfnt.post.numGlyphs > 0 {
			newfnt.post.numGlyphs = uint16(numGlyphs)
		}
		if len(newfnt.post.glyphNameIndex) > numGlyphs {
			newfnt.post.glyphNameIndex = newfnt.post.glyphNameIndex[0:numGlyphs]
		}
		if len(newfnt.post.offsets) > numGlyphs {
			// TODO: Not sure if this is updated here or generated on the fly?
			newfnt.post.offsets = newfnt.post.offsets[0:numGlyphs]
		}
		if len(newfnt.post.glyphNames) > numGlyphs {
			newfnt.post.glyphNames = newfnt.post.glyphNames[0:numGlyphs]
		}
	}
	if f.font.cmap != nil {
		newfnt.cmap = &cmapTable{
			version:   f.cmap.version,
			subtables: map[string]*cmapSubtable{},
		}

		for _, name := range f.cmap.subtableKeys {
			subt := f.cmap.subtables[name]
			switch t := subt.ctx.(type) {
			case cmapSubtableFormat0:
				for i := range t.glyphIDArray {
					if i > numGlyphs {
						t.glyphIDArray[i] = 0
					}
				}
			case cmapSubtableFormat4:
				newt := cmapSubtableFormat4{}
				// Generates a new table: going from glyph index 0 up to numGlyphs.
				// Makes continous entries with deltas.
				// Does not use glyphIDData, but only the deltas.  Can lead to many segments, but should not
				// be too bad (especially since subsetting).
				segments := 0
				i := 0
				for i < numGlyphs {
					j := i + 1
					for ; j < numGlyphs; j++ {
						if int(subt.runes[j]-subt.runes[i]) != j-i {
							break
						}
					}
					// from i:j-1 maps to subt.runes[i]:subt.runes[i]+j-i-1
					startCode := uint16(subt.runes[i])
					endCode := uint16(subt.runes[i]) + uint16(j-i-1)
					idDelta := uint16(uint16(i) - startCode)
					newt.startCode = append(newt.startCode, startCode)
					newt.endCode = append(newt.endCode, endCode)
					newt.idDelta = append(newt.idDelta, idDelta)
					newt.idRangeOffset = append(newt.idRangeOffset, 0)
					segments++
					i = j
				}
				newt.length = uint16(2*8 + 2*4*segments)
				newt.language = t.language
				newt.segCountX2 = uint16(segments * 2)
				newt.searchRange = 2 * uint16(math.Pow(2, math.Floor(math.Log2(float64(segments)))))
				newt.entrySelector = uint16(math.Log2(float64(newt.searchRange) / 2.0))
				newt.rangeShift = uint16(segments*2) - newt.searchRange
				subt.ctx = newt
			case cmapSubtableFormat6:
				for i := range t.glyphIDArray {
					if int(t.glyphIDArray[i]) >= numGlyphs {
						t.glyphIDArray[i] = 0
					}
				}
			case cmapSubtableFormat12:
				newt := cmapSubtableFormat12{}
				groups := 0

				for i := 0; i < numGlyphs; i++ {
					j := i + 1
					for ; j < numGlyphs; j++ {
						if int(subt.runes[j]-subt.runes[i]) != j-i {
							break
						}
					}
					// from i:j-1 maps to subt.runes[i]:subt.runes[i]+j-i-1
					startCharCode := uint32(subt.runes[i])
					endCharCode := uint32(subt.runes[i]) + uint32(j-i-1)
					startGlyphID := uint32(i)

					group := sequentialMapGroup{
						startCharCode: startCharCode,
						endCharCode:   endCharCode,
						startGlyphID:  startGlyphID,
					}
					newt.groups = append(newt.groups, group)
					groups++
				}
				newt.length = uint32(2*2 + 3*4 + groups*3*4)
				newt.language = t.language
				newt.numGroups = uint32(groups)
				subt.ctx = newt
			}

			newfnt.cmap.subtableKeys = append(newfnt.cmap.subtableKeys, name)
			newfnt.cmap.subtables[name] = subt
		}
		newfnt.cmap.numTables = uint16(len(newfnt.cmap.subtables))
	}

	subfnt := &Font{
		br:   nil,
		font: &newfnt,
	}
	return subfnt, nil
}

// Subset creates a subset of `f` including only glyph indices specified by `indices`.
// Returns the new subsetted font, a map of old to new GlyphIndex to GlyphIndex as the removal
// of glyphs requires reordering.
func (f *Font) Subset(indices []GlyphIndex) (newf *Font, oldnew map[GlyphIndex]GlyphIndex, err error) {
	// TODO:
	//     1. Make the new cmap for `runes` if `cmap` is nil, using the cmap table and make a []GlyphIndex
	//        with the glyph indices to keep (index prior to subsetting).
	//     2. Go through each table and leave only data for the glyph indices to be kept.
	return nil, nil, errors.New("not implemented yet")
}

// Write writes the font to `w`.
func (f *Font) Write(w io.Writer) error {
	bw := newByteWriter(w)
	err := f.font.write(bw)
	if err != nil {
		return err
	}
	return bw.flush()
}

// WriteFile writes the font to `outPath`.
func (f *Font) WriteFile(outPath string) error {
	of, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer of.Close()

	return f.Write(of)
}
