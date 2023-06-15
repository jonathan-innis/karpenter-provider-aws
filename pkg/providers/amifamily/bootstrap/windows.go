/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bootstrap

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/samber/lo"
)

type Windows struct {
	Options
}

func (w Windows) Script() (string, error) {
	userData := w.mergeCustomUserData(lo.Compact([]string{lo.FromPtr(w.CustomUserData), w.windowsBootstrapScript()})...)
	return base64.StdEncoding.EncodeToString([]byte(userData)), nil
}

// nolint:gocyclo
func (w Windows) windowsBootstrapScript() string {
	var userData bytes.Buffer
	userData.WriteString("<powershell>\n")
	userData.WriteString("[string]$EKSBootstrapScriptFile = \"$env:ProgramFiles\\Amazon\\EKS\\Start-EKSBootstrap.ps1\"\n")
	userData.WriteString(fmt.Sprintf(`& $EKSBootstrapScriptFile -EKSClusterName '%s' -APIServerEndpoint '%s'`, w.ClusterName, w.ClusterEndpoint))
	if w.CABundle != nil {
		userData.WriteString(fmt.Sprintf(` -Base64ClusterCA '%s'`, *w.CABundle))
	}
	if args := w.kubeletExtraArgs(); len(args) > 0 {
		userData.WriteString(fmt.Sprintf(` -KubeletExtraArgs '%s'`, strings.Join(args, " ")))
	}
	if w.KubeletConfig != nil && len(w.KubeletConfig.ClusterDNS) > 0 {
		userData.WriteString(fmt.Sprintf(` -DNSClusterIP '%s'`, w.KubeletConfig.ClusterDNS[0]))
	}
	if w.KubeletConfig != nil && w.KubeletConfig.ContainerRuntime != nil {
		userData.WriteString(fmt.Sprintf(` -ContainerRuntime '%s'`, *w.KubeletConfig.ContainerRuntime))
	}
	userData.WriteString("\n</powershell>")
	return userData.String()
}

func (w Windows) mergeCustomUserData(userDatas ...string) string {
	var buf bytes.Buffer
	for _, userData := range userDatas {
		buf.Write([]byte(w.formatUserData(userData)))
	}
	return buf.String()
}

// format returns userData in a powershell format
// if the userData passed in is already in a powershell format, then the input is returned without modification
func (w Windows) formatUserData(customUserData string) string {
	if strings.HasPrefix(strings.TrimSpace(customUserData), "<powershell>") {
		return customUserData
	}
	var buf bytes.Buffer
	buf.WriteString("<powershell>\n")
	buf.WriteString(customUserData)
	buf.WriteString("\n</powershell>\n")
	return buf.String()
}
