/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package addonctl

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestNoArgs(t *testing.T) {
	c := &cobra.Command{Use: "addonctl", Args: cobra.NoArgs}

	_, err := c.ExecuteC()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
}

// func TestAddonctlCreate(t *testing.T) { //dryrun
// 	c := &cobra.Command{Use: "addonctl create"}
// 	c.SetArgs([]string{"addon-test"})

// 	output, err := c.ExecuteC()
// 	fmt.Println(output)
// 	if err != nil {
// 		t.Fatalf("Unexpected error: %v", err)
// 	}
// }
