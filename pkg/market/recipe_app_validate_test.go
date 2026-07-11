package market

import "testing"

func TestValidateAppRecipe(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{
			"plain tool (no platforms)",
			"# name: rag\napp pull https://h/rag.tar.gz --name rag;\n",
			true,
		},
		{
			"multi-os consistent",
			"# name: control\napp pull https://h/control-{os}-{arch}.zip --name control --platforms windows/amd64,linux/amd64;\n",
			true,
		},
		{
			"tokens but no --platforms",
			"# name: control\napp pull https://h/control-{os}-{arch}.zip --name control;\n",
			false,
		},
		{
			"--platforms but no tokens",
			"# name: control\napp pull https://h/control.zip --name control --platforms linux/amd64;\n",
			false,
		},
		{
			"malformed platform entry",
			"# name: control\napp pull https://h/control-{os}-{arch}.zip --name control --platforms linuxamd64;\n",
			false,
		},
		{
			"duplicate platform",
			"# name: control\napp pull https://h/control-{os}-{arch}.zip --name control --platforms linux/amd64,linux/amd64;\n",
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAppRecipe(tc.body)
			if (err == nil) != tc.ok {
				t.Errorf("ValidateAppRecipe() err=%v, want ok=%v", err, tc.ok)
			}
		})
	}
}
