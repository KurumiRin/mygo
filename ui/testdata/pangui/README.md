The layout workloads of PanGUI's benchmarks
(https://www.pangui.io/blog/05-layout-rework-and-benchmarks/), as Taffy
keeps them (https://github.com/DioxusLabs/taffy, `benches/pangui`, commit
4e59968eda29ceeb0bbaeb3993c8cd38baa3d0a6) and "Are we layout yet?"
(https://arewelayoutyet.mespin.me, https://github.com/marc2332/arewelayoutyet,
commit 75832447cc8634401fe8a149232cb90b63272951) runs them, unchanged.
`BenchmarkLayoutPanGUI` in `../../layout_pangui_bench_test.go` lays them
out.

Taffy's benchmarks are under the MIT license:

MIT License

Copyright (c) 2018 Visly Inc.
Copyright (c) 2026 Taffy Authors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
