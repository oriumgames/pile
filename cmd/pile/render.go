package main

import (
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"strings"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	out := fs.String("o", "map.png", "output PNG file")
	dimFlag := fs.String("dim", "overworld", "dimension to render")
	bgFlag := fs.String("bg", "", "background color as #rrggbb (default: transparent)")
	boundsFlag := fs.String("bounds", "", "render only the block box x1,z1,x2,z2")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: pile render [-o map.png] [--dim overworld] [--bg #rrggbb] <world-dir>")
	}
	var bg *color.RGBA
	if *bgFlag != "" {
		c, err := parseHexColor(*bgFlag)
		if err != nil {
			return err
		}
		bg = &c
	}
	dim, err := parseDim(*dimFlag)
	if err != nil {
		return err
	}
	reg := world.DefaultBlockRegistry
	reg.Finalize()
	wf, err := pile.LoadWorldFiles(fs.Arg(0), reg, limit.providerOpts()...)
	if err != nil {
		return err
	}
	df := wf.Dim(dim)
	if df == nil || len(df.Columns) == 0 {
		return fmt.Errorf("no chunks in %s", *dimFlag)
	}

	// The span is computed in int64 and the image size is derived from it only
	// after the ceiling test. Subtracting the chunk coordinates as int32 wraps:
	// a world holding one column at X=-2147483648 and one at X=0 has a true
	// span of 2^31+1, which as an int32 difference is -2147483648, so the width
	// came out at -34359738352, sailed past a test that only looks for values
	// above 8192, and image.NewRGBA canonicalised the negative rectangle into a
	// positive one and reserved 2.0 TiB for its pixels. The file that does it is
	// 4,269 bytes and holds two chunks.
	//
	// The box is measured over the columns that will actually draw something,
	// not over every column stored. A column whose surface is air everywhere
	// contributes no pixel, so letting it widen the image adds only transparent
	// margin -- and worlds carry those in bulk: a converted Skywars map held
	// 5 041 empty spawn chunks at the origin and the map itself 87 000 blocks
	// away, which measured 88 192 pixels wide and was refused outright. Had the
	// ceiling been higher it would have written a 412 MB image that was blank
	// apart from one corner.
	// The whole world when no box is given. In block coordinates, not
	// chunk ones: a column at chunk X math.MinInt32 starts at block
	// -34 359 738 368, so an int32-shaped default would place it outside
	// the box that is supposed to mean everything -- silently dropping it
	// here, and in prune silently deleting it.
	x1, z1, x2, z2 := math.MinInt, math.MinInt, math.MaxInt, math.MaxInt
	if *boundsFlag != "" {
		if _, err := fmt.Sscanf(*boundsFlag, "%d,%d,%d,%d", &x1, &z1, &x2, &z2); err != nil {
			return fmt.Errorf("invalid bounds %q: %w", *boundsFlag, err)
		}
		if x2 < x1 {
			x1, x2 = x2, x1
		}
		if z2 < z1 {
			z1, z2 = z2, z1
		}
	}
	air := reg.AirRuntimeID()
	drawn := make([]format.Column, 0, len(df.Columns))
	for _, c := range df.Columns {
		bx0, bx1 := int(c.X)*16, int(c.X)*16+15
		bz0, bz1 := int(c.Z)*16, int(c.Z)*16+15
		if bx1 < x1 || bx0 > x2 || bz1 < z1 || bz0 > z2 {
			continue
		}
		if columnDraws(c.Col.Chunk, air) {
			drawn = append(drawn, c)
		}
	}
	if len(drawn) == 0 {
		if *boundsFlag != "" {
			return fmt.Errorf("nothing to render in %s within (%d,%d)..(%d,%d)",
				*dimFlag, x1, z1, x2, z2)
		}
		return fmt.Errorf("nothing to render in %s: all %d columns are air",
			*dimFlag, len(df.Columns))
	}

	minCX, minCZ := int64(drawn[0].X), int64(drawn[0].Z)
	maxCX, maxCZ := minCX, minCZ
	for _, c := range drawn {
		minCX, maxCX = min(minCX, int64(c.X)), max(maxCX, int64(c.X))
		minCZ, maxCZ = min(minCZ, int64(c.Z)), max(maxCZ, int64(c.Z))
	}
	spanX, spanZ := (maxCX-minCX+1)*16, (maxCZ-minCZ+1)*16
	if spanX > 8192 || spanZ > 8192 {
		return fmt.Errorf("world too large to render (%dx%d blocks); "+
			"its content sits in separate places, so pick one with "+
			"--bounds x1,z1,x2,z2", spanX, spanZ)
	}
	w, h := int(spanX), int(spanZ)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if bg != nil {
		for i := 0; i < len(img.Pix); i += 4 {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = bg.R, bg.G, bg.B, 255
		}
	}

	for _, c := range drawn {
		ch := c.Col.Chunk
		r := ch.Range()
		ox := int((int64(c.X) - minCX) * 16)
		oz := int((int64(c.Z) - minCZ) * 16)
		for x := range uint8(16) {
			for z := range uint8(16) {
				y := ch.HighestBlock(x, z)
				rid := ch.Block(x, y, z, 0)
				if rid == air {
					continue
				}
				name, props, _ := reg.RuntimeIDToState(rid)
				base := blockColor(name, props)
				// Shade by height: higher is brighter.
				f := 0.55 + 0.55*float64(int(y)-r[0])/float64(r.Height())
				img.Set(ox+int(x), oz+int(z), shade(base, f))
			}
		}
	}

	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("rendered %dx%d map to %s\n", w, h, *out)
	return nil
}

// dyeColors maps the 16 Bedrock "color" state property values to display
// colors ("silver" is Bedrock's light gray).
var dyeColors = map[string]color.RGBA{
	"white":      {249, 255, 254, 255},
	"orange":     {249, 128, 29, 255},
	"magenta":    {199, 78, 189, 255},
	"light_blue": {58, 179, 218, 255},
	"yellow":     {254, 216, 61, 255},
	"lime":       {128, 199, 31, 255},
	"pink":       {243, 139, 170, 255},
	"gray":       {71, 79, 82, 255},
	"silver":     {157, 157, 151, 255},
	"light_gray": {157, 157, 151, 255},
	"cyan":       {22, 156, 156, 255},
	"purple":     {137, 50, 184, 255},
	"blue":       {60, 68, 170, 255},
	"brown":      {131, 84, 50, 255},
	"green":      {94, 124, 22, 255},
	"red":        {176, 46, 38, 255},
	"black":      {29, 29, 33, 255},
}

// blockColor picks a display color for a block: the dye color when the state
// has a "color" property (wool, concrete, terracotta, carpet, glass, ...),
// curated colors for common families, deterministic hash color otherwise.
func blockColor(name string, props map[string]any) color.RGBA {
	n := strings.TrimPrefix(name, "minecraft:")
	if cv, ok := props["color"].(string); ok {
		if c, ok := dyeColors[cv]; ok {
			if strings.Contains(n, "terracotta") || strings.Contains(n, "stained_hardened_clay") {
				return shade(c, 0.75) // terracotta variants are muted
			}
			return c
		}
	}
	// Colored blocks that encode the color in the identifier instead of a
	// property (e.g. red_wool, lime_concrete in newer flattened names).
	for dye, c := range dyeColors {
		if strings.HasPrefix(n, dye+"_") {
			if strings.Contains(n, "terracotta") {
				return shade(c, 0.75)
			}
			return c
		}
	}
	for _, e := range []struct {
		sub string
		c   color.RGBA
	}{
		{"water", color.RGBA{63, 118, 228, 255}},
		{"lava", color.RGBA{234, 105, 23, 255}},
		{"grass_block", color.RGBA{121, 176, 76, 255}},
		{"grass", color.RGBA{121, 176, 76, 255}},
		{"leaves", color.RGBA{58, 121, 39, 255}},
		{"snow", color.RGBA{243, 246, 251, 255}},
		{"ice", color.RGBA{160, 200, 255, 255}},
		{"sandstone", color.RGBA{219, 207, 163, 255}},
		{"sand", color.RGBA{229, 217, 173, 255}},
		{"gravel", color.RGBA{136, 130, 127, 255}},
		{"dirt", color.RGBA{134, 96, 67, 255}},
		{"podzol", color.RGBA{106, 76, 45, 255}},
		{"mycelium", color.RGBA{122, 103, 108, 255}},
		{"planks", color.RGBA{162, 130, 78, 255}},
		{"log", color.RGBA{109, 85, 50, 255}},
		{"wood", color.RGBA{109, 85, 50, 255}},
		{"deepslate", color.RGBA{80, 80, 82, 255}},
		{"bedrock", color.RGBA{60, 60, 60, 255}},
		{"obsidian", color.RGBA{21, 18, 30, 255}},
		{"netherrack", color.RGBA{110, 53, 51, 255}},
		{"end_stone", color.RGBA{221, 223, 165, 255}},
		{"stone", color.RGBA{126, 126, 126, 255}},
		{"cobble", color.RGBA{110, 110, 110, 255}},
		{"andesite", color.RGBA{132, 135, 132, 255}},
		{"diorite", color.RGBA{188, 188, 188, 255}},
		{"granite", color.RGBA{149, 103, 86, 255}},
		{"terracotta", color.RGBA{152, 94, 68, 255}},
		{"concrete", color.RGBA{155, 155, 155, 255}},
		{"glass", color.RGBA{200, 230, 240, 255}},
		{"wool", color.RGBA{222, 222, 222, 255}},
		{"quartz", color.RGBA{235, 229, 222, 255}},
		{"brick", color.RGBA{150, 97, 83, 255}},
	} {
		if strings.Contains(n, e.sub) {
			return e.c
		}
	}
	// Deterministic fallback: hash the name into a muted color.
	hs := fnv.New32a()
	_, _ = hs.Write([]byte(n))
	v := hs.Sum32()
	return color.RGBA{
		R: uint8(96 + v%128),
		G: uint8(96 + (v>>8)%128),
		B: uint8(96 + (v>>16)%128),
		A: 255,
	}
}

// parseHexColor parses "#rrggbb".
func parseHexColor(s string) (color.RGBA, error) {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return color.RGBA{}, fmt.Errorf("invalid color %q, want #rrggbb", s)
	}
	var r, g, b uint8
	if _, err := fmt.Sscanf(s, "%02x%02x%02x", &r, &g, &b); err != nil {
		return color.RGBA{}, fmt.Errorf("invalid color %q: %w", s, err)
	}
	return color.RGBA{R: r, G: g, B: b, A: 255}, nil
}

// shade scales a color's brightness by f (clamped).
func shade(c color.RGBA, f float64) color.RGBA {
	cl := func(v float64) uint8 {
		if v > 255 {
			return 255
		}
		if v < 0 {
			return 0
		}
		return uint8(v)
	}
	return color.RGBA{R: cl(float64(c.R) * f), G: cl(float64(c.G) * f), B: cl(float64(c.B) * f), A: 255}
}

// columnDraws reports whether a column would put any pixel in the image: the
// same test the render loop applies, hoisted so the bounding box can be
// measured over the columns that matter rather than over every stored one.
func columnDraws(ch *chunk.Chunk, air uint32) bool {
	for x := range uint8(16) {
		for z := range uint8(16) {
			if ch.Block(x, ch.HighestBlock(x, z), z, 0) != air {
				return true
			}
		}
	}
	return false
}
