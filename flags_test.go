package fig

import (
	"testing"

	"github.com/nbcx/flag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBindFlagValueSet(t *testing.T) {
	Reset()
	flagSet := flag.NewFlagSet("test", flag.ContinueOnError)

	testValues := map[string]*string{
		"host":     nil,
		"port":     nil,
		"endpoint": nil,
	}

	mutatedTestValues := map[string]string{
		"host":     "localhost",
		"port":     "6060",
		"endpoint": "/public",
	}

	for name := range testValues {
		testValues[name] = flagSet.String(name, "", "test")
	}

	flagValueSet := pflagValueSet{flagSet}

	err := BindFlagValues(flagValueSet)
	require.NoError(t, err, "error binding flag set")

	flagSet.VisitAll(func(flag *flag.Flag) {
		flag.Value.Set(mutatedTestValues[flag.Name])
		flag.Changed = true
	})

	for name, expected := range mutatedTestValues {
		assert.Equal(t, expected, Get(name))
	}
}

func TestBindFlagValue(t *testing.T) {
	testString := "testing"
	testValue := newStringValue(testString, &testString)

	flag := &flag.Flag{
		Name:    "testflag",
		Value:   testValue,
		Changed: false,
	}

	flagValue := pflagValue{flag}
	BindFlagValue("testvalue", flagValue)

	assert.Equal(t, testString, Get("testvalue"))

	flag.Value.Set("testing_mutate")
	flag.Changed = true // hack for pflag usage

	assert.Equal(t, "testing_mutate", Get("testvalue"))
}
