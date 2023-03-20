package caddyunmarshal

import "github.com/caddyserver/caddy/v2"

const testCaddyfile = `
	thing1 arg1 {
		foo bar
		baz qux
	} arg2 {
		foo bar
		baz qux
	}

	thing2 arg1 arg2 {
		parameter value
		number 100
		flag
	}

	thing3 /* arg1 arg2 {}
`

// note that ("$1", "$2,optional", "$3") is illegal, because optional arguments
// must be at the end of the argument list.

type thing1 struct {
	Arg1  string            `caddy:"$1"`
	Arg2  string            `caddy:"$3,optional"`
	Junk1 map[string]string `caddy:"{2}"`
	Junk2 map[string]string `caddy:"{4},optional"`
}

// in thing2, Param and Number are implied to be fields of the first block that
// we see.

type thing2 struct {
	Arg1   string `caddy:"$1"`
	Arg2   string `caddy:"$2,optional"`
	Param  string `caddy:"parameter"`
	Number int
	Flag   bool
}

type thing3 struct {
	Matcher caddy.ModuleMap `caddy:"$matcher"`
	Arg1    string          `caddy:"$1"`
	Arg2    string          `caddy:"$2,optional"`
}

// TODO: pointer type support for optionality testing
