package fig

import (
	"fmt"

	"github.com/sagikazarmark/locafero"
	"github.com/spf13/afero"
)

func ExampleFinder() {
	fs := afero.NewMemMapFs()

	fs.Mkdir("/home/user", 0o777)

	f, _ := fs.Create("/home/user/myapp.yaml")
	f.WriteString("foo: bar")
	f.Close()

	// HCL will have a "lower" priority in the search order
	fs.Create("/home/user/myapp.hcl")

	finder := locafero.Finder{
		Paths: []string{"/home/user"},
		Names: locafero.NameWithExtensions("myapp", SupportedExts...),
		Type:  locafero.FileTypeFile, // This is important!
	}

	v := NewWithOptions(WithFinder(finder))
	v.SetFs(fs)
	v.ReadInConfig()

	fmt.Println(v.Get("foo").(string))

	// Output:
	// bar
}

func ExampleFinders() {
	fs := afero.NewMemMapFs()

	fs.Mkdir("/home/user", 0o777)
	f, _ := fs.Create("/home/user/myapp.yaml")
	f.WriteString("foo: bar")
	f.Close()

	fs.Mkdir("/etc/myapp", 0o777)
	fs.Create("/etc/myapp/config.yaml")

	// Combine multiple finders to search for files in multiple locations with different criteria
	finder := Finders(
		locafero.Finder{
			Paths: []string{"/home/user"},
			Names: locafero.NameWithExtensions("myapp", SupportedExts...),
			Type:  locafero.FileTypeFile, // This is important!
		},
		locafero.Finder{
			Paths: []string{"/etc/myapp"},
			Names: []string{"config.yaml"}, // Only accept YAML files in the system config directory
			Type:  locafero.FileTypeFile,   // This is important!
		},
	)

	v := NewWithOptions(WithFinder(finder))
	v.SetFs(fs)
	v.ReadInConfig()

	fmt.Println(v.Get("foo").(string))

	// Output:
	// bar
}
