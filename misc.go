// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file (in go.git repository).

package main

import (
    "flag"
    "fmt"
    "strconv"
)

// flag that is both bool and int - for e.g. handling -v -v -v ...
// inspired/copied by/from cmd.dist.count in go.git
type countFlag int

func (c *countFlag) String() string {
    return fmt.Sprint(int(*c))
}

func (c *countFlag) Set(s string) error {
    switch s {
    case "true":
        *c++
    case "false":
        *c = 0
    default:
        n, err := strconv.Atoi(s)
        if err != nil {
            return fmt.Errorf("invalid count %q", s)
        }
        *c = countFlag(n)
    }
    return nil
}

// flag.boolFlag
func (c *countFlag) IsBoolFlag() bool {
    return true
}

// flag.Value
var _ flag.Value = (*countFlag)(nil)
