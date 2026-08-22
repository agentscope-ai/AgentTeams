# Custom MCP YAML Guide

When a user describes an HTTP API they want to add, generate the YAML config and deploy with `setup-mcp-server.sh --yaml-file`.

## Workflow

1. User describes the API (endpoints, auth, parameters)
2. Generate YAML following the format below
3. Write to `/tmp/mcp-<name>.yaml`
4. Run `setup-mcp-server.sh` with `--yaml-file`

## YAML Structure

```yaml
server:
  name: <server-name>-mcp-server
  config:
    accessToken: ""    # Always use this key — script substitutes the real value
  # allowTools:        # Optional: restrict exposed tools
  #   - tool_name_1

tools:
- name: <tool_name>
  description: "<what this tool does>"
  args:
  - name: <arg_name>
    description: "<arg description>"
    type: string       # string | number | integer | boolean | array | object
    required: true
    # default: <value>
    # For array: items: { type: string }
    # For object: properties: { subfield: { type: string } }
  requestTemplate:
    url: "https://<domain>/path/{{.args.<arg>}}"
    method: GET
    headers:
    - key: Authorization
      value: "Bearer {{.config.accessToken}}"
    # Body options (choose ONE):
    # argsToUrlParam: true     — auto-append all args as query params
    # argsToJsonBody: true     — auto-serialize all args to JSON body
    # argsToFormBody: true     — auto-serialize to form-encoded body
    # body: |                  — manual template (most flexible)
    #   {"param": "{{.args.x}}"}
  # responseTemplate:          # Optional: transform verbose responses
  #   body: |
  #     {{- range $i, $item := .results }}
  #     - **{{$item.name}}**: {{$item.value}}
  #     {{- end }}
```

## Template Syntax (GJSON Template)

| Syntax | Description | Example |
|---|---|---|
| `{{.args.<name>}}` | Reference argument | `{{.args.city}}` |
| `{{.config.<key>}}` | Reference config value | `{{.config.accessToken}}` |
| `{{toJson .args.<name>}}` | Serialize array/object to JSON | `{{toJson .args.filters}}` |
| `{{.args.<str> \| b64enc}}` | Base64-encode | `{{.args.content \| b64enc}}` |
| `{{gjson "path.to.field"}}` | GJSON path on response | `{{gjson "users.#.name"}}` |
| `{{if .args.opt}}...{{end}}` | Conditional | Optional params |
| `{{range .items}}...{{end}}` | Loop | Format lists |
| Sprig functions | `{{add $i 1}}`, `{{upper .args.x}}` | Math, strings |

## Parameter Passing

| Approach | When to use |
|---|---|
| URL with `{{.args.*}}` inline | GET with few params |
| `argsToUrlParam: true` | GET with many query params |
| `argsToJsonBody: true` | POST/PUT with JSON body |
| `argsToFormBody: true` | POST with form-encoded body |
| `body: \|` template | Complex/conditional body |

## Guidelines

1. Tool names: snake_case (`get_forecast`, `search_users`)
2. Clear `description` — LLM uses this to decide which tool to call
3. `required: true` only for truly required params
4. Choose simplest parameter passing that works
5. Add `responseTemplate` only when raw JSON is too verbose
6. Always use `accessToken` as credential key, value `""`

## Example

User: "Add weather API. `GET https://api.openweather.com/v1/weather?q={city}&units={units}`, auth via `X-API-Key` header."

```bash
cat > /tmp/mcp-weather.yaml << 'YAML'
server:
  name: weather-mcp-server
  config:
    accessToken: ""
tools:
- name: get_weather
  description: "Get current weather for a city"
  args:
  - name: city
    description: "City name (e.g., London, Tokyo)"
    type: string
    required: true
  - name: units
    description: "Temperature units"
    type: string
    required: false
    default: "metric"
  requestTemplate:
    url: "https://api.openweather.com/v1/weather?q={{.args.city}}&units={{.args.units}}"
    method: GET
    headers:
    - key: X-API-Key
      value: "{{.config.accessToken}}"
YAML

bash .../setup-mcp-server.sh weather "<key>" --yaml-file /tmp/mcp-weather.yaml
```
