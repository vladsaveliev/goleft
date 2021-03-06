package debiaser

import (
	"fmt"
	"log"
	"sort"

	"github.com/JaderDias/movingmedian"
	"github.com/gonum/floats"
	"github.com/gonum/matrix"
	"github.com/gonum/matrix/mat64"
)

// Debiaser implements inplace removal of bias from a mat64 of (scaled) values.
type Debiaser interface {
	Debias(*mat64.Dense)
}

// Sorter provides method to sort and then unsort a mat64
type Sorter interface {
	Sort(*mat64.Dense)
	Unsort(*mat64.Dense)
}

// SortedDebiaser is useful when we need to: sort, then unbias based on that sort order, then unsort.
// An example of this is for GC bias.
type SortedDebiaser interface {
	Sorter
	Debiaser
}

// GeneralDebiaser is an implementation of a SortedDebiaser that can be used simply by setting attributes.int
// Window is the size of the moving window for correction.
// Usage is to Call GeneralDebiaser.Sort() then Debias(), then Unsort(). Presumbly, Those calls will be flanked
// by a scaler call such as to scaler.ZScore.
type GeneralDebiaser struct {
	Vals   []float64
	Window int
	inds   []int
	tmp    *mat64.Dense
}

func (g *GeneralDebiaser) setTmp(r, c int) {
	if g.tmp == nil {
		g.tmp = mat64.NewDense(r, c, nil)
	} else {
		gr, gc := g.tmp.Dims()
		if gr != r || gc != c {
			g.tmp = mat64.NewDense(r, c, nil)
		}
	}
}

// Sort sorts the rows in mat according the order in g.Vals.
func (g *GeneralDebiaser) Sort(mat *mat64.Dense) {
	if g.inds == nil {
		g.inds = make([]int, len(g.Vals))
	}
	floats.Argsort(g.Vals, g.inds)
	r, c := mat.Dims()
	g.setTmp(r, c)

	changed := false
	for ai, bi := range g.inds {
		if ai != bi {
			changed = true
		}
		g.tmp.SetRow(ai, mat.RawRowView(bi))
	}
	if !changed {
		log.Println("WARNING: no change after sorting. This usually means .Vals is unset or same as previous run")
	}
	// copy g.tmp into mat
	mat.Copy(g.tmp)
}

// Unsort reverts the values to be position sorted.
func (g *GeneralDebiaser) Unsort(mat *mat64.Dense) {
	if g.inds == nil {
		panic("unsort: must call sort first")
	}
	r, c := mat.Dims()
	g.setTmp(r, c)
	// copy mat into g.tmp
	g.tmp.Copy(mat)
	tmp := make([]float64, len(g.Vals))
	for ai, bi := range g.inds {
		mat.SetRow(bi, g.tmp.RawRowView(ai))
		tmp[bi] = g.Vals[ai]
	}
	g.Vals = tmp
}

// Debias by subtracting moving median in each sample.
// It's assumed that g.Sort() has been called before this and that g.Unsort() will be called after.
// It's also assumed that the values in mat have been scaled, for example by a `scaler.ZScore`.
func (g *GeneralDebiaser) Debias(mat *mat64.Dense) {
	r, c := mat.Dims()
	col := make([]float64, r)
	ins := make([]float64, 0, 2000)
	outs := make([]float64, 0, 2000)
	for sampleI := 0; sampleI < c; sampleI++ {
		mat64.Col(col, sampleI, mat)

		mm := movingmedian.NewMovingMedian(g.Window)
		mid := (g.Window-1)/2 + 1
		for i := 0; i < mid; i++ {
			mm.Push(col[i])
			if sampleI == 0 {
				ins = append(ins, col[i])
			}
		}
		for i := 0; i < mid; i++ {
			col[i] -= mm.Median()
			if sampleI == 0 {
				outs = append(outs, col[i])
			}
		}

		var i int
		for i = mid; i < len(col)-mid; i++ {
			mm.Push(col[i+mid])
			col[i] -= mm.Median()
			if sampleI == 0 {
				outs = append(outs, col[i])
				ins = append(ins, col[i])
			}
		}
		for ; i < len(col); i++ {
			col[i] -= mm.Median()
		}
		mat.SetCol(sampleI, col)
	}
}

type ChunkDebiaser struct {
	GeneralDebiaser
	// ScoreWindow defines the range of Vals used per window.
	// E.g. if this is 0.1 then all values from 0.25-0.35 will be normalized to the median of
	// Depths occuring in that range.
	ScoreWindow float64
}

func (cd *ChunkDebiaser) Debias(mat *mat64.Dense) {
	if cd.ScoreWindow == 0 {
		panic("must set ChunkDebiaser.ScoreWindow")
	}
	r, c := mat.Dims()
	col := make([]float64, r)

	slices := make([]int, 1, 100)
	v0 := cd.Vals[0]
	for i := 0; i < len(cd.Vals); i++ {
		if cd.Vals[i]-v0 > cd.ScoreWindow {
			v0 = cd.Vals[i]
			slices = append(slices, i)
		}
	}
	slices = append(slices, len(cd.Vals))
	dpSubset := make([]float64, 0, len(cd.Vals))

	for sampleI := 0; sampleI < c; sampleI++ {
		mat64.Col(col, sampleI, mat)
		for i := 1; i < len(slices); i++ {
			si, ei := slices[i-1], slices[i]
			dpSubset = dpSubset[:(ei - si)]
			copy(dpSubset, col[si:ei])
			sort.Float64s(dpSubset)
			median := dpSubset[(ei-si)/2]

			for j := si; j < ei; j++ {
				col[j] /= median
			}
		}
		mat.SetCol(sampleI, col)
	}
}

type SVD struct {
	MinVariancePct float64
}

func (isvd *SVD) Debias(mat *mat64.Dense) {
	var svd mat64.SVD
	if ok := svd.Factorize(mat, matrix.SVDThin); !ok {
		panic("error with SVD")
	}

	// get svd and zero out first n components.
	s, u, v := extractSVD(&svd)
	sum := floats.Sum(s)
	str := "variance:"

	var n int
	for n = 0; n < 15 && 100*s[n]/sum > isvd.MinVariancePct; n++ {
		str += fmt.Sprintf(" %.2f", 100*s[n]/sum)
	}
	log.Println(str)

	sigma := mat64.NewDense(len(s), len(s), nil)
	// leave the first n as 0 and set the rest.
	for i := n; i < len(s); i++ {
		sigma.Set(i, i, s[i])
	}
	mat.Product(u, sigma, v.T())
}

func extractSVD(svd *mat64.SVD) (s []float64, u, v *mat64.Dense) {
	var um, vm mat64.Dense
	um.UFromSVD(svd)
	vm.VFromSVD(svd)
	s = svd.Values(nil)
	return s, &um, &vm
}
