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
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/afero"
	"github.com/spf13/cast"

	"github.com/nbcx/fig/internal/features"
	"github.com/nbcx/flag"
	"github.com/nbcx/go-kit/to"
)

// ConfigMarshalError happens when failing to marshal the configuration.
type ConfigMarshalError struct {
	err error
}

// Error returns the formatted configuration error.
func (e ConfigMarshalError) Error() string {
	return fmt.Sprintf("While marshaling config: %s", e.err.Error())
}

// UnsupportedConfigError denotes encountering an unsupported
// configuration filetype.
type UnsupportedConfigError string

// Error returns the formatted configuration error.
func (str UnsupportedConfigError) Error() string {
	return fmt.Sprintf("Unsupported Config Type %q", string(str))
}

// ConfigFileNotFoundError denotes failing to find configuration file.
type ConfigFileNotFoundError struct {
	name, locations string
}

// Error returns the formatted configuration error.
func (fnfe ConfigFileNotFoundError) Error() string {
	return fmt.Sprintf("Config File %q Not Found in %q", fnfe.name, fnfe.locations)
}

// ConfigFileAlreadyExistsError denotes failure to write new configuration file.
type ConfigFileAlreadyExistsError string

// Error returns the formatted error when configuration already exists.
func (faee ConfigFileAlreadyExistsError) Error() string {
	return fmt.Sprintf("Config File %q Already Exists", string(faee))
}

// A DecoderConfigOption can be passed to viper.Unmarshal to configure
// mapstructure.DecoderConfig options.
type DecoderConfigOption func(*mapstructure.DecoderConfig)

// DecodeHook returns a DecoderConfigOption which overrides the default
// DecoderConfig.DecodeHook value, the default is:
//
//	 mapstructure.ComposeDecodeHookFunc(
//			mapstructure.StringToTimeDurationHookFunc(),
//			mapstructure.StringToSliceHookFunc(","),
//		)
func DecodeHook(hook mapstructure.DecodeHookFunc) DecoderConfigOption {
	return func(c *mapstructure.DecoderConfig) {
		c.DecodeHook = hook
	}
}

// Viper is a prioritized configuration registry. It
// maintains a set of configuration sources, fetches
// values to populate those, and provides them according
// to the source's priority.
// The priority of the sources is the following:
// 1. overrides
// 2. flags
// 3. env. variables
// 4. config file
// 5. key/value store
// 6. defaults
//
// For example, if values from the following sources were loaded:
//
//	Defaults : {
//		"secret": "",
//		"user": "default",
//		"endpoint": "https://localhost"
//	}
//	Config : {
//		"user": "root"
//		"secret": "defaultsecret"
//	}
//	Env : {
//		"secret": "somesecretkey"
//	}
//
// The resulting config will have the following values:
//
//	{
//		"secret": "somesecretkey",
//		"user": "root",
//		"endpoint": "https://localhost"
//	}
//
// Note: Vipers are not safe for concurrent Get() and Set() operations.
type Viper struct {
	// Delimiter that separates a list of keys
	// used to access a nested value in one go
	keyDelim string

	// A set of paths to look for the config file in
	configPaths []string

	// The filesystem to read config from.
	fs afero.Fs

	finder Finder

	// A set of remote providers to search for the configuration
	remoteProviders []*defaultRemoteProvider

	// Name of file to look for inside the path
	configName        string
	configFile        string
	configType        string
	configPermissions os.FileMode
	envPrefix         string

	automaticEnvApplied bool
	envKeyReplacer      StringReplacer
	allowEmptyEnv       bool

	parents        []string
	config         map[string]any
	override       map[string]any
	defaults       map[string]any
	kvStore        map[string]any
	flags          map[string]FlagValue
	env            map[string][]string
	aliases        map[string]string
	typeByDefValue bool

	onConfigChange func(fsnotify.Event)

	logger *slog.Logger

	encoderRegistry EncoderRegistry
	decoderRegistry DecoderRegistry

	decodeHook mapstructure.DecodeHookFunc

	experimentalFinder     bool
	experimentalBindStruct bool
}

// New returns an initialized Viper instance.
func New() *Viper {
	v := new(Viper)
	v.keyDelim = "."
	v.configName = "config"
	v.configPermissions = os.FileMode(0o644)
	v.fs = afero.NewOsFs()
	v.config = make(map[string]any)
	v.parents = []string{}
	v.override = make(map[string]any)
	v.defaults = make(map[string]any)
	v.kvStore = make(map[string]any)
	v.flags = make(map[string]FlagValue)
	v.env = make(map[string][]string)
	v.aliases = make(map[string]string)
	v.typeByDefValue = false
	v.logger = slog.New(&discardHandler{})

	codecRegistry := NewCodecRegistry()

	v.encoderRegistry = codecRegistry
	v.decoderRegistry = codecRegistry

	v.experimentalFinder = features.Finder
	v.experimentalBindStruct = features.BindStruct

	return v
}

// Option configures Viper using the functional options paradigm popularized by Rob Pike and Dave Cheney.
// If you're unfamiliar with this style,
// see https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html and
// https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis.
type Option interface {
	apply(v *Viper)
}

type optionFunc func(v *Viper)

func (fn optionFunc) apply(v *Viper) {
	fn(v)
}

// KeyDelimiter sets the delimiter used for determining key parts.
// By default it's value is ".".
func KeyDelimiter(d string) Option {
	return optionFunc(func(v *Viper) {
		v.keyDelim = d
	})
}

// StringReplacer applies a set of replacements to a string.
type StringReplacer interface {
	// Replace returns a copy of s with all replacements performed.
	Replace(s string) string
}

// EnvKeyReplacer sets a replacer used for mapping environment variables to internal keys.
func EnvKeyReplacer(r StringReplacer) Option {
	return optionFunc(func(v *Viper) {
		if r == nil {
			return
		}

		v.envKeyReplacer = r
	})
}

// WithDecodeHook sets a default decode hook for mapstructure.
func WithDecodeHook(h mapstructure.DecodeHookFunc) Option {
	return optionFunc(func(v *Viper) {
		if h == nil {
			return
		}

		v.decodeHook = h
	})
}

// NewWithOptions creates a new Viper instance.
func NewWithOptions(opts ...Option) *Viper {
	v := New()

	for _, opt := range opts {
		opt.apply(v)
	}

	return v
}

// SetOptions sets the options on the global Viper instance.
//
// Be careful when using this function: subsequent calls may override options you set.
// It's always better to use a local Viper instance.
func SetOptions(opts ...Option) {
	for _, opt := range opts {
		opt.apply(v)
	}
}

// SupportedExts are universally supported extensions.
var SupportedExts = []string{"json", "toml", "yaml", "yml", "properties", "props", "prop", "hcl", "tfvars", "dotenv", "env", "ini"}

// OnConfigChange sets the event handler that is called when a config file changes.
func (v *Viper) OnConfigChange(run func(in fsnotify.Event)) {
	v.onConfigChange = run
}

// WatchConfig starts watching a config file for changes.
// WatchConfig starts watching a config file for changes.
func (v *Viper) WatchConfig() {
	initWG := sync.WaitGroup{}
	initWG.Add(1)
	go func() {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			v.logger.Error(fmt.Sprintf("failed to create watcher: %s", err))
			os.Exit(1)
		}
		defer watcher.Close()
		// we have to watch the entire directory to pick up renames/atomic saves in a cross-platform way
		filename, err := v.getConfigFile()
		if err != nil {
			v.logger.Error(fmt.Sprintf("get config file: %s", err))
			initWG.Done()
			return
		}

		configFile := filepath.Clean(filename)
		configDir, _ := filepath.Split(configFile)
		realConfigFile, _ := filepath.EvalSymlinks(filename)

		eventsWG := sync.WaitGroup{}
		eventsWG.Add(1)
		go func() {
			for {
				select {
				case event, ok := <-watcher.Events:
					if !ok { // 'Events' channel is closed
						eventsWG.Done()
						return
					}
					currentConfigFile, _ := filepath.EvalSymlinks(filename)
					// we only care about the config file with the following cases:
					// 1 - if the config file was modified or created
					// 2 - if the real path to the config file changed (eg: k8s ConfigMap replacement)
					if (filepath.Clean(event.Name) == configFile &&
						(event.Has(fsnotify.Write) || event.Has(fsnotify.Create))) ||
						(currentConfigFile != "" && currentConfigFile != realConfigFile) {
						realConfigFile = currentConfigFile
						err := v.ReadInConfig()
						if err != nil {
							v.logger.Error(fmt.Sprintf("read config file: %s", err))
						}
						if v.onConfigChange != nil {
							v.onConfigChange(event)
						}
					} else if filepath.Clean(event.Name) == configFile && event.Has(fsnotify.Remove) {
						eventsWG.Done()
						return
					}

				case err, ok := <-watcher.Errors:
					if ok { // 'Errors' channel is not closed
						v.logger.Error(fmt.Sprintf("watcher error: %s", err))
					}
					eventsWG.Done()
					return
				}
			}
		}()
		watcher.Add(configDir)
		initWG.Done()   // done initializing the watch in this go routine, so the parent routine can move on...
		eventsWG.Wait() // now, wait for event loop to end in this go-routine...
	}()
	initWG.Wait() // make sure that the go routine above fully ended before returning
}

// SetConfigFile explicitly defines the path, name and extension of the config file.
// Viper will use this and not check any of the config paths.
func (v *Viper) SetConfigFile(in string) {
	if in != "" {
		v.configFile = in
	}
}

// SetEnvPrefix defines a prefix that ENVIRONMENT variables will use.
// E.g. if your prefix is "spf", the env registry will look for env
// variables that start with "SPF_".
func (v *Viper) SetEnvPrefix(in string) {
	if in != "" {
		v.envPrefix = in
	}
}

func (v *Viper) GetEnvPrefix() string {
	return v.envPrefix
}

func (v *Viper) mergeWithEnvPrefix(in string) string {
	if v.envPrefix != "" {
		return strings.ToUpper(v.envPrefix + "_" + in)
	}

	return strings.ToUpper(in)
}

// AllowEmptyEnv tells Viper to consider set,
// but empty environment variables as valid values instead of falling back.
// For backward compatibility reasons this is false by default.
func (v *Viper) AllowEmptyEnv(allowEmptyEnv bool) {
	v.allowEmptyEnv = allowEmptyEnv
}

// TODO: should getEnv logic be moved into find(). Can generalize the use of
// rewriting keys many things, Ex: Get('someKey') -> some_key
// (camel case to snake case for JSON keys perhaps)

// getEnv is a wrapper around os.Getenv which replaces characters in the original
// key. This allows env vars which have different keys than the config object
// keys.
func (v *Viper) getEnv(key string) (string, bool) {
	if v.envKeyReplacer != nil {
		key = v.envKeyReplacer.Replace(key)
	}

	val, ok := os.LookupEnv(key)

	return val, ok && (v.allowEmptyEnv || val != "")
}

// ConfigFileUsed returns the file used to populate the config registry.
func (v *Viper) ConfigFileUsed() string { return v.configFile }

// AddConfigPath adds a path for Viper to search for the config file in.
// Can be called multiple times to define multiple search paths.
func (v *Viper) AddConfigPath(in string) {
	if v.finder != nil {
		v.logger.Warn("ineffective call to function: custom finder takes precedence", slog.String("function", "AddConfigPath"))
	}

	if in != "" {
		absin := absPathify(v.logger, in)

		v.logger.Info("adding path to search paths", "path", absin)
		if !slices.Contains(v.configPaths, absin) {
			v.configPaths = append(v.configPaths, absin)
		}
	}
}

// searchMap recursively searches for a value for path in source map.
// Returns nil if not found.
// Note: This assumes that the path entries and map keys are lower cased.
func (v *Viper) searchMap(source map[string]any, path []string) any {
	if len(path) == 0 {
		return source
	}

	next, ok := source[path[0]]
	if ok {
		// Fast path
		if len(path) == 1 {
			return next
		}

		// Nested case
		switch next := next.(type) {
		case map[any]any:
			return v.searchMap(cast.ToStringMap(next), path[1:])
		case map[string]any:
			// Type assertion is safe here since it is only reached
			// if the type of `next` is the same as the type being asserted
			return v.searchMap(next, path[1:])
		default:
			// got a value but nested key expected, return "nil" for not found
			return nil
		}
	}
	return nil
}

// searchIndexableWithPathPrefixes recursively searches for a value for path in source map/slice.
//
// While searchMap() considers each path element as a single map key or slice index, this
// function searches for, and prioritizes, merged path elements.
// e.g., if in the source, "foo" is defined with a sub-key "bar", and "foo.bar"
// is also defined, this latter value is returned for path ["foo", "bar"].
//
// This should be useful only at config level (other maps may not contain dots
// in their keys).
//
// Note: This assumes that the path entries and map keys are lower cased.
func (v *Viper) searchIndexableWithPathPrefixes(source any, path []string) any {
	if len(path) == 0 {
		return source
	}

	// search for path prefixes, starting from the longest one
	for i := len(path); i > 0; i-- {
		prefixKey := strings.ToLower(strings.Join(path[0:i], v.keyDelim))

		var val any
		switch sourceIndexable := source.(type) {
		case []any:
			val = v.searchSliceWithPathPrefixes(sourceIndexable, prefixKey, i, path)
		case map[string]any:
			val = v.searchMapWithPathPrefixes(sourceIndexable, prefixKey, i, path)
		}
		if val != nil {
			return val
		}
	}

	// not found
	return nil
}

// searchSliceWithPathPrefixes searches for a value for path in sourceSlice
//
// This function is part of the searchIndexableWithPathPrefixes recurring search and
// should not be called directly from functions other than searchIndexableWithPathPrefixes.
func (v *Viper) searchSliceWithPathPrefixes(
	sourceSlice []any,
	prefixKey string,
	pathIndex int,
	path []string,
) any {
	// if the prefixKey is not a number or it is out of bounds of the slice
	index, err := strconv.Atoi(prefixKey)
	if err != nil || len(sourceSlice) <= index {
		return nil
	}

	next := sourceSlice[index]

	// Fast path
	if pathIndex == len(path) {
		return next
	}

	switch n := next.(type) {
	case map[any]any:
		return v.searchIndexableWithPathPrefixes(cast.ToStringMap(n), path[pathIndex:])
	case map[string]any, []any:
		return v.searchIndexableWithPathPrefixes(n, path[pathIndex:])
	default:
		// got a value but nested key expected, do nothing and look for next prefix
	}

	// not found
	return nil
}

// searchMapWithPathPrefixes searches for a value for path in sourceMap
//
// This function is part of the searchIndexableWithPathPrefixes recurring search and
// should not be called directly from functions other than searchIndexableWithPathPrefixes.
func (v *Viper) searchMapWithPathPrefixes(
	sourceMap map[string]any,
	prefixKey string,
	pathIndex int,
	path []string,
) any {
	next, ok := sourceMap[prefixKey]
	if !ok {
		return nil
	}

	// Fast path
	if pathIndex == len(path) {
		return next
	}

	// Nested case
	switch n := next.(type) {
	case map[any]any:
		return v.searchIndexableWithPathPrefixes(cast.ToStringMap(n), path[pathIndex:])
	case map[string]any, []any:
		return v.searchIndexableWithPathPrefixes(n, path[pathIndex:])
	default:
		// got a value but nested key expected, do nothing and look for next prefix
	}

	// not found
	return nil
}

// isPathShadowedInDeepMap makes sure the given path is not shadowed somewhere
// on its path in the map.
// e.g., if "foo.bar" has a value in the given map, it “shadows”
//
//	"foo.bar.baz" in a lower-priority map
func (v *Viper) isPathShadowedInDeepMap(path []string, m map[string]any) string {
	var parentVal any
	for i := 1; i < len(path); i++ {
		parentVal = v.searchMap(m, path[0:i])
		if parentVal == nil {
			// not found, no need to add more path elements
			return ""
		}
		switch parentVal.(type) {
		case map[any]any:
			continue
		case map[string]any:
			continue
		default:
			// parentVal is a regular value which shadows "path"
			return strings.Join(path[0:i], v.keyDelim)
		}
	}
	return ""
}

// isPathShadowedInFlatMap makes sure the given path is not shadowed somewhere
// in a sub-path of the map.
// e.g., if "foo.bar" has a value in the given map, it “shadows”
//
//	"foo.bar.baz" in a lower-priority map
func (v *Viper) isPathShadowedInFlatMap(path []string, mi any) string {
	// unify input map
	var m map[string]interface{}
	switch miv := mi.(type) {
	case map[string]string:
		m = castMapStringToMapInterface(miv)
	case map[string]FlagValue:
		m = castMapFlagToMapInterface(miv)
	default:
		return ""
	}

	// scan paths
	var parentKey string
	for i := 1; i < len(path); i++ {
		parentKey = strings.Join(path[0:i], v.keyDelim)
		if _, ok := m[parentKey]; ok {
			return parentKey
		}
	}
	return ""
}

// isPathShadowedInAutoEnv makes sure the given path is not shadowed somewhere
// in the environment, when automatic env is on.
// e.g., if "foo.bar" has a value in the environment, it “shadows”
//
//	"foo.bar.baz" in a lower-priority map
func (v *Viper) isPathShadowedInAutoEnv(path []string) string {
	var parentKey string
	for i := 1; i < len(path); i++ {
		parentKey = strings.Join(path[0:i], v.keyDelim)
		if _, ok := v.getEnv(v.mergeWithEnvPrefix(parentKey)); ok {
			return parentKey
		}
	}
	return ""
}

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
func (v *Viper) SetTypeByDefaultValue(enable bool) {
	v.typeByDefValue = enable
}

// Get can retrieve any value given the key to use.
// Get is case-insensitive for a key.
// Get has the behavior of returning the value associated with the first
// place from where it is set. Viper will check in the following order:
// override, flag, env, config file, key/value store, default
//
// Get returns an interface. For a specific value use one of the Get____ methods.
func (v *Viper) Get(key string, def ...any) (vl *to.Value) {
	if val := v.get(key); val != nil {
		return to.V(val)
	}
	if len(def) > 0 {
		return to.V(def[0])
	}
	return vl
}

// Must always able to obtain a string
func (v *Viper) Must(key string, def ...any) string {
	return v.Get(key, def...).String()
}

func (v *Viper) get(key string) any {
	lcaseKey := strings.ToLower(key)
	val := v.find(lcaseKey, true)
	if val == nil {
		return nil
	}

	if v.typeByDefValue {
		// TODO(bep) this branch isn't covered by a single test.
		valType := val
		path := strings.Split(lcaseKey, v.keyDelim)
		defVal := v.searchMap(v.defaults, path)
		if defVal != nil {
			valType = defVal
		}

		switch valType.(type) {
		case bool:
			return cast.ToBool(val)
		case string:
			return cast.ToString(val)
		case int32, int16, int8, int:
			return cast.ToInt(val)
		case uint:
			return cast.ToUint(val)
		case uint32:
			return cast.ToUint32(val)
		case uint64:
			return cast.ToUint64(val)
		case int64:
			return cast.ToInt64(val)
		case float64, float32:
			return cast.ToFloat64(val)
		case time.Time:
			return cast.ToTime(val)
		case time.Duration:
			return cast.ToDuration(val)
		case []string:
			return cast.ToStringSlice(val)
		case []int:
			return cast.ToIntSlice(val)
		case []time.Duration:
			return cast.ToDurationSlice(val)
		}
	}

	return val
}

// Sub returns new Viper instance representing a sub tree of this instance.
// Sub is case-insensitive for a key.
func (v *Viper) Sub(key string) *Viper {
	subv := New()
	data := v.get(key)
	if data == nil {
		return nil
	}

	if reflect.TypeOf(data).Kind() == reflect.Map {
		subv.parents = append([]string(nil), v.parents...)
		subv.parents = append(subv.parents, strings.ToLower(key))
		subv.automaticEnvApplied = v.automaticEnvApplied
		subv.envPrefix = v.envPrefix
		subv.envKeyReplacer = v.envKeyReplacer
		subv.keyDelim = v.keyDelim
		subv.config = cast.ToStringMap(data)
		return subv
	}
	return nil
}

// UnmarshalKey takes a single key and unmarshals it into a Struct.
func (v *Viper) UnmarshalKey(key string, rawVal any, opts ...DecoderConfigOption) error {
	return decode(v.Get(key), v.defaultDecoderConfig(rawVal, opts...))
}

// Unmarshal unmarshals the config into a Struct. Make sure that the tags
// on the fields of the structure are properly set.
func (v *Viper) Unmarshal(rawVal any, opts ...DecoderConfigOption) error {
	keys := v.AllKeys()

	if v.experimentalBindStruct {
		// TODO: make this optional?
		structKeys, err := v.decodeStructKeys(rawVal, opts...)
		if err != nil {
			return err
		}

		keys = append(keys, structKeys...)
	}

	// TODO: struct keys should be enough?
	return decode(v.getSettings(keys), v.defaultDecoderConfig(rawVal, opts...))
}

func (v *Viper) decodeStructKeys(input any, opts ...DecoderConfigOption) ([]string, error) {
	var structKeyMap map[string]any

	err := decode(input, v.defaultDecoderConfig(&structKeyMap, opts...))
	if err != nil {
		return nil, err
	}

	flattenedStructKeyMap := v.flattenAndMergeMap(map[string]bool{}, structKeyMap, "")

	r := make([]string, 0, len(flattenedStructKeyMap))
	for v := range flattenedStructKeyMap {
		r = append(r, v)
	}

	return r, nil
}

// defaultDecoderConfig returns default mapstructure.DecoderConfig with support
// of time.Duration values & string slices.
func (v *Viper) defaultDecoderConfig(output any, opts ...DecoderConfigOption) *mapstructure.DecoderConfig {
	decodeHook := v.decodeHook
	if decodeHook == nil {
		decodeHook = mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			// mapstructure.StringToSliceHookFunc(","),
			stringToWeakSliceHookFunc(","),
		)
	}

	c := &mapstructure.DecoderConfig{
		Metadata:         nil,
		WeaklyTypedInput: true,
		DecodeHook:       decodeHook,
	}

	for _, opt := range opts {
		opt(c)
	}

	// Do not allow overwriting the output
	c.Result = output

	return c
}

// UnmarshalExact unmarshals the config into a Struct, erroring if a field is nonexistent
// in the destination struct.
func (v *Viper) UnmarshalExact(rawVal any, opts ...DecoderConfigOption) error {
	config := v.defaultDecoderConfig(rawVal, opts...)
	config.ErrorUnused = true

	keys := v.AllKeys()

	if v.experimentalBindStruct {
		// TODO: make this optional?
		structKeys, err := v.decodeStructKeys(rawVal, opts...)
		if err != nil {
			return err
		}

		keys = append(keys, structKeys...)
	}

	// TODO: struct keys should be enough?
	return decode(v.getSettings(keys), config)
}

// BindPFlags binds a full flag set to the configuration, using each flag's long
// name as the config key.
func (v *Viper) BindPFlags(flags *flag.FlagSet) error {
	return v.BindFlagValues(pflagValueSet{flags})
}

// BindPFlag binds a specific key to a pflag (as used by cobra).
// Example (where serverCmd is a Cobra instance):
//
//	serverCmd.Flags().Int("port", 1138, "Port to run Application server on")
//	Viper.BindPFlag("port", serverCmd.Flags().Lookup("port"))
func (v *Viper) BindPFlag(key string, flag *flag.Flag) error {
	if flag == nil {
		return fmt.Errorf("flag for %q is nil", key)
	}
	return v.BindFlagValue(key, pflagValue{flag})
}

// BindFlagValues binds a full FlagValue set to the configuration, using each flag's long
// name as the config key.
func (v *Viper) BindFlagValues(flags FlagValueSet) (err error) {
	flags.VisitAll(func(flag FlagValue) {
		if err = v.BindFlagValue(flag.Name(), flag); err != nil {
			return
		}
	})
	return nil
}

// BindFlagValue binds a specific key to a FlagValue.
func (v *Viper) BindFlagValue(key string, flag FlagValue) error {
	if flag == nil {
		return fmt.Errorf("flag for %q is nil", key)
	}
	v.flags[strings.ToLower(key)] = flag
	return nil
}

// BindEnv binds a Viper key to a ENV variable.
// ENV variables are case sensitive.
// If only a key is provided, it will use the env key matching the key, uppercased.
// If more arguments are provided, they will represent the env variable names that
// should bind to this key and will be taken in the specified order.
// EnvPrefix will be used when set when env name is not provided.
func (v *Viper) BindEnv(input ...string) error {
	if len(input) == 0 {
		return fmt.Errorf("missing key to bind to")
	}

	key := strings.ToLower(input[0])

	if len(input) == 1 {
		v.env[key] = append(v.env[key], v.mergeWithEnvPrefix(key))
	} else {
		v.env[key] = append(v.env[key], input[1:]...)
	}

	return nil
}

// MustBindEnv wraps BindEnv in a panic.
// If there is an error binding an environment variable, MustBindEnv will
// panic.
func (v *Viper) MustBindEnv(input ...string) {
	if err := v.BindEnv(input...); err != nil {
		panic(fmt.Sprintf("error while binding environment variable: %v", err))
	}
}

// Given a key, find the value.
//
// Viper will check to see if an alias exists first.
// Viper will then check in the following order:
// flag, env, config file, key/value store.
// Lastly, if no value was found and flagDefault is true, and if the key
// corresponds to a flag, the flag's default value is returned.
//
// Note: this assumes a lower-cased key given.
func (v *Viper) find(lcaseKey string, flagDefault bool) any {
	var (
		val    any
		exists bool
		path   = strings.Split(lcaseKey, v.keyDelim)
		nested = len(path) > 1
	)

	// compute the path through the nested maps to the nested value
	if nested && v.isPathShadowedInDeepMap(path, castMapStringToMapInterface(v.aliases)) != "" {
		return nil
	}

	// if the requested key is an alias, then return the proper key
	lcaseKey = v.realKey(lcaseKey)
	path = strings.Split(lcaseKey, v.keyDelim)
	nested = len(path) > 1

	// Set() override first
	val = v.searchMap(v.override, path)
	if val != nil {
		return val
	}
	if nested && v.isPathShadowedInDeepMap(path, v.override) != "" {
		return nil
	}

	// PFlag override next
	flag, exists := v.flags[lcaseKey]
	if exists && flag.HasChanged() {
		switch flag.ValueType() {
		case "int", "int8", "int16", "int32", "int64":
			return cast.ToInt(flag.ValueString())
		case "bool":
			return cast.ToBool(flag.ValueString())
		case "stringSlice", "stringArray":
			s := strings.TrimPrefix(flag.ValueString(), "[")
			s = strings.TrimSuffix(s, "]")
			res, _ := readAsCSV(s)
			return res
		case "intSlice":
			s := strings.TrimPrefix(flag.ValueString(), "[")
			s = strings.TrimSuffix(s, "]")
			res, _ := readAsCSV(s)
			return cast.ToIntSlice(res)
		case "durationSlice":
			s := strings.TrimPrefix(flag.ValueString(), "[")
			s = strings.TrimSuffix(s, "]")
			slice := strings.Split(s, ",")
			return cast.ToDurationSlice(slice)
		case "stringToString":
			return stringToStringConv(flag.ValueString())
		case "stringToInt":
			return stringToIntConv(flag.ValueString())
		default:
			return flag.ValueString()
		}
	}
	if nested && v.isPathShadowedInFlatMap(path, v.flags) != "" {
		return nil
	}

	// Env override next
	if v.automaticEnvApplied {
		envKey := strings.Join(append(v.parents, lcaseKey), ".")
		// even if it hasn't been registered, if automaticEnv is used,
		// check any Get request
		if val, ok := v.getEnv(v.mergeWithEnvPrefix(envKey)); ok {
			return val
		}
		if nested && v.isPathShadowedInAutoEnv(path) != "" {
			return nil
		}
	}
	envkeys, exists := v.env[lcaseKey]
	if exists {
		for _, envkey := range envkeys {
			if val, ok := v.getEnv(envkey); ok {
				return val
			}
		}
	}
	if nested && v.isPathShadowedInFlatMap(path, v.env) != "" {
		return nil
	}

	// Config file next
	val = v.searchIndexableWithPathPrefixes(v.config, path)
	if val != nil {
		return val
	}
	if nested && v.isPathShadowedInDeepMap(path, v.config) != "" {
		return nil
	}

	// K/V store next
	val = v.searchMap(v.kvStore, path)
	if val != nil {
		return val
	}
	if nested && v.isPathShadowedInDeepMap(path, v.kvStore) != "" {
		return nil
	}

	// Default next
	val = v.searchMap(v.defaults, path)
	if val != nil {
		return val
	}
	if nested && v.isPathShadowedInDeepMap(path, v.defaults) != "" {
		return nil
	}

	if flagDefault {
		// last chance: if no value is found and a flag does exist for the key,
		// get the flag's default value even if the flag's value has not been set.
		if flag, exists := v.flags[lcaseKey]; exists {
			switch flag.ValueType() {
			case "int", "int8", "int16", "int32", "int64":
				return cast.ToInt(flag.ValueString())
			case "bool":
				return cast.ToBool(flag.ValueString())
			case "stringSlice", "stringArray":
				s := strings.TrimPrefix(flag.ValueString(), "[")
				s = strings.TrimSuffix(s, "]")
				res, _ := readAsCSV(s)
				return res
			case "intSlice":
				s := strings.TrimPrefix(flag.ValueString(), "[")
				s = strings.TrimSuffix(s, "]")
				res, _ := readAsCSV(s)
				return cast.ToIntSlice(res)
			case "stringToString":
				return stringToStringConv(flag.ValueString())
			case "stringToInt":
				return stringToIntConv(flag.ValueString())
			case "durationSlice":
				s := strings.TrimPrefix(flag.ValueString(), "[")
				s = strings.TrimSuffix(s, "]")
				slice := strings.Split(s, ",")
				return cast.ToDurationSlice(slice)
			default:
				return flag.ValueString()
			}
		}
		// last item, no need to check shadowing
	}

	return nil
}

// IsSet checks to see if the key has been set in any of the data locations.
// IsSet is case-insensitive for a key.
func (v *Viper) IsSet(key string) bool {
	lcaseKey := strings.ToLower(key)
	val := v.find(lcaseKey, false)
	return val != nil
}

// AutomaticEnv makes Viper check if environment variables match any of the existing keys
// (config, default or flags). If matching env vars are found, they are loaded into Viper.
func (v *Viper) AutomaticEnv() {
	v.automaticEnvApplied = true
}

// SetEnvKeyReplacer sets the strings.Replacer on the viper object
// Useful for mapping an environmental variable to a key that does
// not match it.
func (v *Viper) SetEnvKeyReplacer(r *strings.Replacer) {
	v.envKeyReplacer = r
}

// RegisterAlias creates an alias that provides another accessor for the same key.
// This enables one to change a name without breaking the application.
func (v *Viper) RegisterAlias(alias, key string) {
	v.registerAlias(alias, strings.ToLower(key))
}

func (v *Viper) registerAlias(alias, key string) {
	alias = strings.ToLower(alias)
	if alias != key && alias != v.realKey(key) {
		_, exists := v.aliases[alias]

		if !exists {
			// if we alias something that exists in one of the maps to another
			// name, we'll never be able to get that value using the original
			// name, so move the config value to the new realkey.
			if val, ok := v.config[alias]; ok {
				delete(v.config, alias)
				v.config[key] = val
			}
			if val, ok := v.kvStore[alias]; ok {
				delete(v.kvStore, alias)
				v.kvStore[key] = val
			}
			if val, ok := v.defaults[alias]; ok {
				delete(v.defaults, alias)
				v.defaults[key] = val
			}
			if val, ok := v.override[alias]; ok {
				delete(v.override, alias)
				v.override[key] = val
			}
			v.aliases[alias] = key
		}
	} else {
		v.logger.Warn("creating circular reference alias", "alias", alias, "key", key, "real_key", v.realKey(key))
	}
}

func (v *Viper) realKey(key string) string {
	newkey, exists := v.aliases[key]
	if exists {
		v.logger.Debug("key is an alias", "alias", key, "to", newkey)

		return v.realKey(newkey)
	}
	return key
}

// InConfig checks to see if the given key (or an alias) is in the config file.
func (v *Viper) InConfig(key string) bool {
	lcaseKey := strings.ToLower(key)

	// if the requested key is an alias, then return the proper key
	lcaseKey = v.realKey(lcaseKey)
	path := strings.Split(lcaseKey, v.keyDelim)

	return v.searchIndexableWithPathPrefixes(v.config, path) != nil
}

// SetDefault sets the default value for this key.
// SetDefault is case-insensitive for a key.
// Default only used when no value is provided by the user via flag, config or ENV.
func (v *Viper) SetDefault(key string, value any) {
	// If alias passed in, then set the proper default
	key = v.realKey(strings.ToLower(key))
	value = toCaseInsensitiveValue(value)

	path := strings.Split(key, v.keyDelim)
	lastKey := strings.ToLower(path[len(path)-1])
	deepestMap := deepSearch(v.defaults, path[0:len(path)-1])

	// set innermost value
	deepestMap[lastKey] = value
}

// Set sets the value for the key in the override register.
// Set is case-insensitive for a key.
// Will be used instead of values obtained via
// flags, config file, ENV, default, or key/value store.
func (v *Viper) Set(key string, value any) {
	// If alias passed in, then set the proper override
	key = v.realKey(strings.ToLower(key))
	value = toCaseInsensitiveValue(value)

	path := strings.Split(key, v.keyDelim)
	lastKey := strings.ToLower(path[len(path)-1])
	deepestMap := deepSearch(v.override, path[0:len(path)-1])

	// set innermost value
	deepestMap[lastKey] = value
}

// ReadInConfig will discover and load the configuration file from disk
// and key/value stores, searching in one of the defined paths.
func (v *Viper) ReadInConfig() error {
	v.logger.Info("attempting to read in config file")
	filename, err := v.getConfigFile()
	if err != nil {
		return err
	}

	if !slices.Contains(SupportedExts, v.getConfigType()) {
		return UnsupportedConfigError(v.getConfigType())
	}

	v.logger.Debug("reading file", "file", filename)
	file, err := afero.ReadFile(v.fs, filename)
	if err != nil {
		return err
	}

	config := make(map[string]any)

	err = v.unmarshalReader(bytes.NewReader(file), config)
	if err != nil {
		return err
	}

	v.config = config
	return nil
}

// MergeInConfig merges a new configuration with an existing config.
func (v *Viper) MergeInConfig() error {
	v.logger.Info("attempting to merge in config file")
	filename, err := v.getConfigFile()
	if err != nil {
		return err
	}

	if !slices.Contains(SupportedExts, v.getConfigType()) {
		return UnsupportedConfigError(v.getConfigType())
	}

	file, err := afero.ReadFile(v.fs, filename)
	if err != nil {
		return err
	}

	return v.MergeConfig(bytes.NewReader(file))
}

// ReadConfig will read a configuration file, setting existing keys to nil if the
// key does not exist in the file.
func (v *Viper) ReadConfig(in io.Reader) error {
	if v.configType == "" {
		return errors.New("cannot decode configuration: config type is not set")
	}

	v.config = make(map[string]any)
	return v.unmarshalReader(in, v.config)
}

// MergeConfig merges a new configuration with an existing config.
func (v *Viper) MergeConfig(in io.Reader) error {
	if v.configType == "" {
		return errors.New("cannot decode configuration: config type is not set")
	}

	cfg := make(map[string]any)
	if err := v.unmarshalReader(in, cfg); err != nil {
		return err
	}
	return v.MergeConfigMap(cfg)
}

// MergeConfigMap merges the configuration from the map given with an existing config.
// Note that the map given may be modified.
func (v *Viper) MergeConfigMap(cfg map[string]any) error {
	if v.config == nil {
		v.config = make(map[string]any)
	}
	insensitiviseMap(cfg)
	mergeMaps(cfg, v.config, nil)
	return nil
}

// WriteConfig writes the current configuration to a file.
func (v *Viper) WriteConfig() error {
	filename, err := v.getConfigFile()
	if err != nil {
		return err
	}
	return v.writeConfig(filename, true)
}

// SafeWriteConfig writes current configuration to file only if the file does not exist.
func (v *Viper) SafeWriteConfig() error {
	if len(v.configPaths) < 1 {
		return errors.New("missing configuration for 'configPath'")
	}
	return v.SafeWriteConfigAs(filepath.Join(v.configPaths[0], v.configName+"."+v.configType))
}

// WriteConfigAs writes current configuration to a given filename.
func (v *Viper) WriteConfigAs(filename string) error {
	return v.writeConfig(filename, true)
}

// WriteConfigTo writes current configuration to an [io.Writer].
func (v *Viper) WriteConfigTo(w io.Writer) error {
	format := strings.ToLower(v.getConfigType())

	if !slices.Contains(SupportedExts, format) {
		return UnsupportedConfigError(format)
	}

	return v.marshalWriter(w, format)
}

// SafeWriteConfigAs writes current configuration to a given filename if it does not exist.
func (v *Viper) SafeWriteConfigAs(filename string) error {
	alreadyExists, err := afero.Exists(v.fs, filename)
	if alreadyExists && err == nil {
		return ConfigFileAlreadyExistsError(filename)
	}
	return v.writeConfig(filename, false)
}

func (v *Viper) writeConfig(filename string, force bool) error {
	v.logger.Info("attempting to write configuration to file")

	var configType string

	ext := filepath.Ext(filename)
	if ext != "" && ext != filepath.Base(filename) {
		configType = ext[1:]
	} else {
		configType = v.configType
	}
	if configType == "" {
		return fmt.Errorf("config type could not be determined for %s", filename)
	}

	if !slices.Contains(SupportedExts, configType) {
		return UnsupportedConfigError(configType)
	}
	if v.config == nil {
		v.config = make(map[string]any)
	}
	flags := os.O_CREATE | os.O_TRUNC | os.O_WRONLY
	if !force {
		flags |= os.O_EXCL
	}
	f, err := v.fs.OpenFile(filename, flags, v.configPermissions)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := v.marshalWriter(f, configType); err != nil {
		return err
	}

	return f.Sync()
}

func (v *Viper) unmarshalReader(in io.Reader, c map[string]any) error {
	buf := new(bytes.Buffer)
	buf.ReadFrom(in)

	format := strings.ToLower(v.getConfigType())

	if !slices.Contains(SupportedExts, format) {
		return UnsupportedConfigError(format)
	}

	decoder, err := v.decoderRegistry.Decoder(format)
	if err != nil {
		return ConfigParseError{err}
	}

	err = decoder.Decode(buf.Bytes(), c)
	if err != nil {
		return ConfigParseError{err}
	}

	insensitiviseMap(c)
	return nil
}

// Marshal a map into Writer.
func (v *Viper) marshalWriter(w io.Writer, configType string) error {
	c := v.AllSettings()

	encoder, err := v.encoderRegistry.Encoder(configType)
	if err != nil {
		return ConfigMarshalError{err}
	}

	b, err := encoder.Encode(c)
	if err != nil {
		return ConfigMarshalError{err}
	}

	_, err = w.Write(b)
	if err != nil {
		return ConfigMarshalError{err}
	}

	return nil
}

func keyExists(k string, m map[string]any) string {
	lk := strings.ToLower(k)
	for mk := range m {
		lmk := strings.ToLower(mk)
		if lmk == lk {
			return mk
		}
	}
	return ""
}

func castToMapStringInterface(
	src map[any]any,
) map[string]any {
	tgt := map[string]any{}
	for k, v := range src {
		tgt[fmt.Sprintf("%v", k)] = v
	}
	return tgt
}

func castMapStringSliceToMapInterface(src map[string][]string) map[string]any {
	tgt := map[string]any{}
	for k, v := range src {
		tgt[k] = v
	}
	return tgt
}

func castMapStringToMapInterface(src map[string]string) map[string]any {
	tgt := map[string]any{}
	for k, v := range src {
		tgt[k] = v
	}
	return tgt
}

func castMapFlagToMapInterface(src map[string]FlagValue) map[string]any {
	tgt := map[string]any{}
	for k, v := range src {
		tgt[k] = v
	}
	return tgt
}

// mergeMaps merges two maps. The `itgt` parameter is for handling go-yaml's
// insistence on parsing nested structures as `map[any]any`
// instead of using a `string` as the key for nest structures beyond one level
// deep. Both map types are supported as there is a go-yaml fork that uses
// `map[string]any` instead.
func mergeMaps(src, tgt map[string]any, itgt map[any]any) {
	for sk, sv := range src {
		tk := keyExists(sk, tgt)
		if tk == "" {
			v.logger.Debug("", "tk", "\"\"", fmt.Sprintf("tgt[%s]", sk), sv)
			tgt[sk] = sv
			if itgt != nil {
				itgt[sk] = sv
			}
			continue
		}

		tv, ok := tgt[tk]
		if !ok {
			v.logger.Debug("", fmt.Sprintf("ok[%s]", tk), false, fmt.Sprintf("tgt[%s]", sk), sv)
			tgt[sk] = sv
			if itgt != nil {
				itgt[sk] = sv
			}
			continue
		}

		svType := reflect.TypeOf(sv)
		tvType := reflect.TypeOf(tv)

		v.logger.Debug(
			"processing",
			"key", sk,
			"st", svType,
			"tt", tvType,
			"sv", sv,
			"tv", tv,
		)

		switch ttv := tv.(type) {
		case map[any]any:
			v.logger.Debug("merging maps (must convert)")
			tsv, ok := sv.(map[any]any)
			if !ok {
				v.logger.Error(
					"Could not cast sv to map[any]any",
					"key", sk,
					"st", svType,
					"tt", tvType,
					"sv", sv,
					"tv", tv,
				)
				continue
			}

			ssv := castToMapStringInterface(tsv)
			stv := castToMapStringInterface(ttv)
			mergeMaps(ssv, stv, ttv)
		case map[string]any:
			v.logger.Debug("merging maps")
			tsv, ok := sv.(map[string]any)
			if !ok {
				v.logger.Error(
					"Could not cast sv to map[string]any",
					"key", sk,
					"st", svType,
					"tt", tvType,
					"sv", sv,
					"tv", tv,
				)
				continue
			}
			mergeMaps(tsv, ttv, nil)
		default:
			v.logger.Debug("setting value")
			tgt[tk] = sv
			if itgt != nil {
				itgt[tk] = sv
			}
		}
	}
}

// AllKeys returns all keys holding a value, regardless of where they are set.
// Nested keys are returned with a v.keyDelim separator.
func (v *Viper) AllKeys() []string {
	m := map[string]bool{}
	// add all paths, by order of descending priority to ensure correct shadowing
	m = v.flattenAndMergeMap(m, castMapStringToMapInterface(v.aliases), "")
	m = v.flattenAndMergeMap(m, v.override, "")
	m = v.mergeFlatMap(m, castMapFlagToMapInterface(v.flags))
	m = v.mergeFlatMap(m, castMapStringSliceToMapInterface(v.env))
	m = v.flattenAndMergeMap(m, v.config, "")
	m = v.flattenAndMergeMap(m, v.kvStore, "")
	m = v.flattenAndMergeMap(m, v.defaults, "")

	// convert set of paths to list
	a := make([]string, 0, len(m))
	for x := range m {
		a = append(a, x)
	}
	return a
}

// flattenAndMergeMap recursively flattens the given map into a map[string]bool
// of key paths (used as a set, easier to manipulate than a []string):
//   - each path is merged into a single key string, delimited with v.keyDelim
//   - if a path is shadowed by an earlier value in the initial shadow map,
//     it is skipped.
//
// The resulting set of paths is merged to the given shadow set at the same time.
func (v *Viper) flattenAndMergeMap(shadow map[string]bool, m map[string]any, prefix string) map[string]bool {
	if shadow != nil && prefix != "" && shadow[prefix] {
		// prefix is shadowed => nothing more to flatten
		return shadow
	}
	if shadow == nil {
		shadow = make(map[string]bool)
	}

	var m2 map[string]any
	if prefix != "" {
		prefix += v.keyDelim
	}
	for k, val := range m {
		fullKey := prefix + k
		switch val := val.(type) {
		case map[string]any:
			m2 = val
		case map[any]any:
			m2 = cast.ToStringMap(val)
		default:
			// immediate value
			shadow[strings.ToLower(fullKey)] = true
			continue
		}
		// recursively merge to shadow map
		shadow = v.flattenAndMergeMap(shadow, m2, fullKey)
	}
	return shadow
}

// mergeFlatMap merges the given maps, excluding values of the second map
// shadowed by values from the first map.
func (v *Viper) mergeFlatMap(shadow map[string]bool, m map[string]any) map[string]bool {
	// scan keys
outer:
	for k := range m {
		path := strings.Split(k, v.keyDelim)
		// scan intermediate paths
		var parentKey string
		for i := 1; i < len(path); i++ {
			parentKey = strings.Join(path[0:i], v.keyDelim)
			if shadow[parentKey] {
				// path is shadowed, continue
				continue outer
			}
		}
		// add key
		shadow[strings.ToLower(k)] = true
	}
	return shadow
}

// AllSettings merges all settings and returns them as a map[string]any.
func (v *Viper) AllSettings() map[string]any {
	return v.getSettings(v.AllKeys())
}

func (v *Viper) getSettings(keys []string) map[string]any {
	m := map[string]any{}
	// start from the list of keys, and construct the map one value at a time
	for _, k := range keys {
		value := v.get(k)
		if value == nil {
			// should not happen, since AllKeys() returns only keys holding a value,
			// check just in case anything changes
			continue
		}
		path := strings.Split(k, v.keyDelim)
		lastKey := strings.ToLower(path[len(path)-1])
		deepestMap := deepSearch(m, path[0:len(path)-1])
		// set innermost value
		deepestMap[lastKey] = value
	}
	return m
}

// SetFs sets the filesystem to use to read configuration.
func (v *Viper) SetFs(fs afero.Fs) {
	v.fs = fs
}

// SetConfigName sets name for the config file.
// Does not include extension.
func (v *Viper) SetConfigName(in string) {
	if v.finder != nil {
		v.logger.Warn("ineffective call to function: custom finder takes precedence", slog.String("function", "SetConfigName"))
	}

	if in != "" {
		v.configName = in
		v.configFile = ""
	}
}

// SetConfigType sets the type of the configuration returned by the
// remote source, e.g. "json".
func (v *Viper) SetConfigType(in string) {
	if in != "" {
		v.configType = in
	}
}

// SetConfigPermissions sets the permissions for the config file.
func (v *Viper) SetConfigPermissions(perm os.FileMode) {
	v.configPermissions = perm.Perm()
}

func (v *Viper) getConfigType() string {
	if v.configType != "" {
		return v.configType
	}

	cf, err := v.getConfigFile()
	if err != nil {
		return ""
	}

	ext := filepath.Ext(cf)

	if len(ext) > 1 {
		return ext[1:]
	}

	return ""
}

func (v *Viper) getConfigFile() (string, error) {
	if v.configFile == "" {
		cf, err := v.findConfigFile()
		if err != nil {
			return "", err
		}
		v.configFile = cf
	}
	return v.configFile, nil
}

// Debug prints all configuration registries for debugging
// purposes.
func (v *Viper) Debug() { v.DebugTo(os.Stdout) }

func (v *Viper) DebugTo(w io.Writer) {
	fmt.Fprintf(w, "Aliases:\n%#v\n", v.aliases)
	fmt.Fprintf(w, "Override:\n%#v\n", v.override)
	fmt.Fprintf(w, "PFlags:\n%#v\n", v.flags)
	fmt.Fprintf(w, "Env:\n%#v\n", v.env)
	fmt.Fprintf(w, "Key/Value Store:\n%#v\n", v.kvStore)
	fmt.Fprintf(w, "Config:\n%#v\n", v.config)
	fmt.Fprintf(w, "Defaults:\n%#v\n", v.defaults)
}
