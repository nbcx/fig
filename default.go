// Viper is an application configuration system.
// It believes that applications can be configured a variety of ways
// via flags, ENVIRONMENT variables, configuration files retrieved
// from the file system, or a remote key/value store.

// Each item takes precedence over the item below it:

// overrides
// flag
// env
// config
// key/value store
// default

package fig

import (
	"encoding/csv"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/afero"

	"github.com/nbcx/flag"
	"github.com/nbcx/go-kit/to"
)

var v *Viper

func init() {
	v = New()
}

// Reset is intended for testing, will reset all to default settings.
// In the public interface for the viper package so applications
// can use it in their testing as well.
func Reset() {
	v = New()
	SupportedExts = []string{"json", "toml", "yaml", "yml", "properties", "props", "prop", "hcl", "tfvars", "dotenv", "env", "ini"}

	resetRemote()
}

// WatchConfig starts watching a config file for changes.
func WatchConfig() { v.WatchConfig() }

// SetConfigFile explicitly defines the path, name and extension of the config file.
// Viper will use this and not check any of the config paths.
func SetConfigFile(in string) { v.SetConfigFile(in) }

// SetEnvPrefix defines a prefix that ENVIRONMENT variables will use.
// E.g. if your prefix is "spf", the env registry will look for env
// variables that start with "SPF_".
func SetEnvPrefix(in string) { v.SetEnvPrefix(in) }

func GetEnvPrefix() string { return v.GetEnvPrefix() }

// OnConfigChange sets the event handler that is called when a config file changes.
func OnConfigChange(run func(in fsnotify.Event)) { v.OnConfigChange(run) }

// AllowEmptyEnv tells Viper to consider set,
// but empty environment variables as valid values instead of falling back.
// For backward compatibility reasons this is false by default.
func AllowEmptyEnv(allowEmptyEnv bool) { v.AllowEmptyEnv(allowEmptyEnv) }

// ConfigFileUsed returns the file used to populate the config registry.
func ConfigFileUsed() string { return v.ConfigFileUsed() }

// AddConfigPath adds a path for Viper to search for the config file in.
// Can be called multiple times to define multiple search paths.
func AddConfigPath(in string) { v.AddConfigPath(in) }

// SetTypeByDefaultValue enables or disables the inference of a key value's
// type when the Get function is used based upon a key's default value as
// opposed to the value returned based on the normal fetch logic.
//
// For example, if a key has a default value of []string{} and the same key
// is set via an environment variable to "a b c", a call to the Get function
// would return a string slice for the key if the key's type is inferred by
// the default value and the Get function would return:
//
//	[]string {"a", "b", "c"}
//
// Otherwise the Get function would return:
//
//	"a b c"
func SetTypeByDefaultValue(enable bool) { v.SetTypeByDefaultValue(enable) }

// GetViper gets the global Viper instance.
func GetViper() *Viper {
	return v
}

// Get can retrieve any value given the key to use.
// Get is case-insensitive for a key.
// Get has the behavior of returning the value associated with the first
// place from where it is set. Viper will check in the following order:
// override, flag, env, config file, key/value store, default
//
// Get returns an interface. For a specific value use one of the Get____ methods.
func Get(key string, def ...any) *to.Value { return v.Get(key, def...) }

// Sub returns new Viper instance representing a sub tree of this instance.
// Sub is case-insensitive for a key.
func Sub(key string) *Viper { return v.Sub(key) }

// UnmarshalKey takes a single key and unmarshals it into a Struct.
func UnmarshalKey(key string, rawVal any, opts ...DecoderConfigOption) error {
	return v.UnmarshalKey(key, rawVal, opts...)
}

// Unmarshal unmarshals the config into a Struct. Make sure that the tags
// on the fields of the structure are properly set.
func Unmarshal(rawVal any, opts ...DecoderConfigOption) error {
	return v.Unmarshal(rawVal, opts...)
}

// As of mapstructure v2.0.0 StringToSliceHookFunc checks if the return type is a string slice.
// This function removes that check.
// TODO: implement a function that checks if the value can be converted to the return type and use it instead.
func stringToWeakSliceHookFunc(sep string) mapstructure.DecodeHookFunc {
	return func(
		f reflect.Type,
		t reflect.Type,
		data interface{},
	) (interface{}, error) {
		if f.Kind() != reflect.String || t.Kind() != reflect.Slice {
			return data, nil
		}

		raw := data.(string)
		if raw == "" {
			return []string{}, nil
		}

		return strings.Split(raw, sep), nil
	}
}

// decode is a wrapper around mapstructure.Decode that mimics the WeakDecode functionality.
func decode(input any, config *mapstructure.DecoderConfig) error {
	decoder, err := mapstructure.NewDecoder(config)
	if err != nil {
		return err
	}
	return decoder.Decode(input)
}

// UnmarshalExact unmarshals the config into a Struct, erroring if a field is nonexistent
// in the destination struct.
func UnmarshalExact(rawVal any, opts ...DecoderConfigOption) error {
	return v.UnmarshalExact(rawVal, opts...)
}

// BindPFlags binds a full flag set to the configuration, using each flag's long
// name as the config key.
func BindPFlags(flags *flag.FlagSet) error { return v.BindPFlags(flags) }

// BindPFlag binds a specific key to a pflag (as used by cobra).
// Example (where serverCmd is a Cobra instance):
//
//	serverCmd.Flags().Int("port", 1138, "Port to run Application server on")
//	Viper.BindPFlag("port", serverCmd.Flags().Lookup("port"))
func BindPFlag(key string, flag *flag.Flag) error { return v.BindPFlag(key, flag) }

// BindFlagValues binds a full FlagValue set to the configuration, using each flag's long
// name as the config key.
func BindFlagValues(flags FlagValueSet) error { return v.BindFlagValues(flags) }

// BindFlagValue binds a specific key to a FlagValue.
func BindFlagValue(key string, flag FlagValue) error { return v.BindFlagValue(key, flag) }

// BindEnv binds a Viper key to a ENV variable.
// ENV variables are case sensitive.
// If only a key is provided, it will use the env key matching the key, uppercased.
// If more arguments are provided, they will represent the env variable names that
// should bind to this key and will be taken in the specified order.
// EnvPrefix will be used when set when env name is not provided.
func BindEnv(input ...string) error { return v.BindEnv(input...) }

// MustBindEnv wraps BindEnv in a panic.
// If there is an error binding an environment variable, MustBindEnv will
// panic.
func MustBindEnv(input ...string) { v.MustBindEnv(input...) }

func readAsCSV(val string) ([]string, error) {
	if val == "" {
		return []string{}, nil
	}
	stringReader := strings.NewReader(val)
	csvReader := csv.NewReader(stringReader)
	return csvReader.Read()
}

// mostly copied from pflag's implementation of this operation here https://github.com/nbcx/flag/blob/master/string_to_string.go#L79
// alterations are: errors are swallowed, map[string]any is returned in order to enable cast.ToStringMap.
func stringToStringConv(val string) any {
	val = strings.Trim(val, "[]")
	// An empty string would cause an empty map
	if val == "" {
		return map[string]any{}
	}
	r := csv.NewReader(strings.NewReader(val))
	ss, err := r.Read()
	if err != nil {
		return nil
	}
	out := make(map[string]any, len(ss))
	for _, pair := range ss {
		k, vv, found := strings.Cut(pair, "=")
		if !found {
			return nil
		}
		out[k] = vv
	}
	return out
}

// mostly copied from pflag's implementation of this operation here https://github.com/nbcx/flag/blob/d5e0c0615acee7028e1e2740a11102313be88de1/string_to_int.go#L68
// alterations are: errors are swallowed, map[string]any is returned in order to enable cast.ToStringMap.
func stringToIntConv(val string) any {
	val = strings.Trim(val, "[]")
	// An empty string would cause an empty map
	if val == "" {
		return map[string]any{}
	}
	ss := strings.Split(val, ",")
	out := make(map[string]any, len(ss))
	for _, pair := range ss {
		k, vv, found := strings.Cut(pair, "=")
		if !found {
			return nil
		}
		var err error
		out[k], err = strconv.Atoi(vv)
		if err != nil {
			return nil
		}
	}
	return out
}

// IsSet checks to see if the key has been set in any of the data locations.
// IsSet is case-insensitive for a key.
func IsSet(key string) bool { return v.IsSet(key) }

// AutomaticEnv makes Viper check if environment variables match any of the existing keys
// (config, default or flags). If matching env vars are found, they are loaded into Viper.
func AutomaticEnv() { v.AutomaticEnv() }

// SetEnvKeyReplacer sets the strings.Replacer on the viper object
// Useful for mapping an environmental variable to a key that does
// not match it.
func SetEnvKeyReplacer(r *strings.Replacer) { v.SetEnvKeyReplacer(r) }

// RegisterAlias creates an alias that provides another accessor for the same key.
// This enables one to change a name without breaking the application.
func RegisterAlias(alias, key string) { v.RegisterAlias(alias, key) }

// InConfig checks to see if the given key (or an alias) is in the config file.
func InConfig(key string) bool { return v.InConfig(key) }

// SetDefault sets the default value for this key.
// SetDefault is case-insensitive for a key.
// Default only used when no value is provided by the user via flag, config or ENV.
func SetDefault(key string, value any) { v.SetDefault(key, value) }

// Set sets the value for the key in the override register.
// Set is case-insensitive for a key.
// Will be used instead of values obtained via
// flags, config file, ENV, default, or key/value store.
func Set(key string, value any) { v.Set(key, value) }

// ReadInConfig will discover and load the configuration file from disk
// and key/value stores, searching in one of the defined paths.
func ReadInConfig() error { return v.ReadInConfig() }

// MergeInConfig merges a new configuration with an existing config.
func MergeInConfig() error { return v.MergeInConfig() }

// ReadConfig will read a configuration file, setting existing keys to nil if the
// key does not exist in the file.
func ReadConfig(in io.Reader) error { return v.ReadConfig(in) }

// MergeConfig merges a new configuration with an existing config.
func MergeConfig(in io.Reader) error { return v.MergeConfig(in) }

// MergeConfigMap merges the configuration from the map given with an existing config.
// Note that the map given may be modified.
func MergeConfigMap(cfg map[string]any) error { return v.MergeConfigMap(cfg) }

// WriteConfig writes the current configuration to a file.
func WriteConfig() error { return v.WriteConfig() }

// SafeWriteConfig writes current configuration to file only if the file does not exist.
func SafeWriteConfig() error { return v.SafeWriteConfig() }

// WriteConfigAs writes current configuration to a given filename.
func WriteConfigAs(filename string) error { return v.WriteConfigAs(filename) }

// WriteConfigTo writes current configuration to an [io.Writer].
func WriteConfigTo(w io.Writer) error { return v.WriteConfigTo(w) }

// SafeWriteConfigAs writes current configuration to a given filename if it does not exist.
func SafeWriteConfigAs(filename string) error { return v.SafeWriteConfigAs(filename) }

// AllKeys returns all keys holding a value, regardless of where they are set.
// Nested keys are returned with a v.keyDelim separator.
func AllKeys() []string { return v.AllKeys() }

// AllSettings merges all settings and returns them as a map[string]any.
func AllSettings() map[string]any { return v.AllSettings() }

// SetFs sets the filesystem to use to read configuration.
func SetFs(fs afero.Fs) { v.SetFs(fs) }

// SetConfigName sets name for the config file.
// Does not include extension.
func SetConfigName(in string) { v.SetConfigName(in) }

// SetConfigType sets the type of the configuration returned by the
// remote source, e.g. "json".
func SetConfigType(in string) { v.SetConfigType(in) }

// SetConfigPermissions sets the permissions for the config file.
func SetConfigPermissions(perm os.FileMode) { v.SetConfigPermissions(perm) }

// Debug prints all configuration registries for debugging
// purposes.
func Debug()              { v.Debug() }
func DebugTo(w io.Writer) { v.DebugTo(w) }
