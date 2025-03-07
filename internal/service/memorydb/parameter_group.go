package memorydb

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/memorydb"
	"github.com/hashicorp/aws-sdk-go-base/tfawserr"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/hashicorp/terraform-provider-aws/internal/conns"
	"github.com/hashicorp/terraform-provider-aws/internal/create"
	tftags "github.com/hashicorp/terraform-provider-aws/internal/tags"
	"github.com/hashicorp/terraform-provider-aws/internal/tfresource"
	"github.com/hashicorp/terraform-provider-aws/internal/verify"
)

func ResourceParameterGroup() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceParameterGroupCreate,
		ReadContext:   resourceParameterGroupRead,
		UpdateContext: resourceParameterGroupUpdate,
		DeleteContext: resourceParameterGroupDelete,

		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		CustomizeDiff: verify.SetTagsDiff,

		Schema: map[string]*schema.Schema{
			"arn": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"description": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
				Default:  "Managed by Terraform",
			},
			"family": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"name": {
				Type:          schema.TypeString,
				Optional:      true,
				Computed:      true,
				ForceNew:      true,
				ConflictsWith: []string{"name_prefix"},
				ValidateFunc: validation.All(
					validation.StringLenBetween(1, 255),
					validation.StringDoesNotMatch(
						regexp.MustCompile(`[-][-]`),
						"The name may not contain two consecutive hyphens."),
					validation.StringMatch(
						// Similar to ElastiCache, MemoryDB normalises names to lowercase.
						regexp.MustCompile(`^[a-z0-9-]*[a-z0-9]$`),
						"Only lowercase alphanumeric characters and hyphens allowed. The name may not end with a hyphen."),
				),
			},
			"name_prefix": {
				Type:          schema.TypeString,
				Optional:      true,
				Computed:      true,
				ForceNew:      true,
				ConflictsWith: []string{"name"},
				ValidateFunc: validation.All(
					validation.StringLenBetween(1, 255-resource.UniqueIDSuffixLength),
					validation.StringDoesNotMatch(
						regexp.MustCompile(`[-][-]`),
						"The name may not contain two consecutive hyphens."),
					validation.StringMatch(
						// Similar to ElastiCache, MemoryDB normalises names to lowercase.
						regexp.MustCompile(`^[a-z0-9-]+$`),
						"Only lowercase alphanumeric characters and hyphens allowed."),
				),
			},
			"parameter": {
				Type:     schema.TypeSet,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"name": {
							Type:     schema.TypeString,
							Required: true,
						},
						"value": {
							Type:     schema.TypeString,
							Required: true,
						},
					},
				},
				Set: ParameterHash,
			},
			"tags":     tftags.TagsSchema(),
			"tags_all": tftags.TagsSchemaComputed(),
		},
	}
}

func resourceParameterGroupCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	conn := meta.(*conns.AWSClient).MemoryDBConn
	defaultTagsConfig := meta.(*conns.AWSClient).DefaultTagsConfig
	tags := defaultTagsConfig.MergeTags(tftags.New(d.Get("tags").(map[string]interface{})))

	name := create.Name(d.Get("name").(string), d.Get("name_prefix").(string))
	input := &memorydb.CreateParameterGroupInput{
		Description:        aws.String(d.Get("description").(string)),
		Family:             aws.String(d.Get("family").(string)),
		ParameterGroupName: aws.String(name),
		Tags:               Tags(tags.IgnoreAWS()),
	}

	log.Printf("[DEBUG] Creating MemoryDB Parameter Group: %s", input)
	output, err := conn.CreateParameterGroupWithContext(ctx, input)

	if err != nil {
		return diag.Errorf("error creating MemoryDB Parameter Group (%s): %s", name, err)
	}

	d.SetId(name)
	d.Set("arn", output.ParameterGroup.ARN)

	log.Printf("[INFO] MemoryDB Parameter Group ID: %s", d.Id())

	// Update to apply parameter changes.
	return resourceParameterGroupUpdate(ctx, d, meta)
}

func resourceParameterGroupUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	conn := meta.(*conns.AWSClient).MemoryDBConn

	if d.HasChange("parameter") {
		o, n := d.GetChange("parameter")
		toRemove, toAdd := ParameterChanges(o, n)

		log.Printf("[DEBUG] Updating MemoryDB Parameter Group (%s)", d.Id())
		log.Printf("[DEBUG] Parameters to remove: %#v", toRemove)
		log.Printf("[DEBUG] Parameters to add or update: %#v", toAdd)

		// The API is limited to updating no more than 20 parameters at a time.
		const maxParams = 20

		for len(toRemove) > 0 {
			// Removing a parameter from state is equivalent to resetting it
			// to its default state.

			var paramsToReset []*memorydb.ParameterNameValue
			if len(toRemove) <= maxParams {
				paramsToReset, toRemove = toRemove[:], nil
			} else {
				paramsToReset, toRemove = toRemove[:maxParams], toRemove[maxParams:]
			}

			err := resetParameterGroupParameters(ctx, conn, d.Get("name").(string), paramsToReset)

			if err != nil {
				return diag.Errorf("error resetting MemoryDB Parameter Group (%s) parameters to defaults: %s", d.Id(), err)
			}
		}

		for len(toAdd) > 0 {
			var paramsToModify []*memorydb.ParameterNameValue
			if len(toAdd) <= maxParams {
				paramsToModify, toAdd = toAdd[:], nil
			} else {
				paramsToModify, toAdd = toAdd[:maxParams], toAdd[maxParams:]
			}

			err := modifyParameterGroupParameters(ctx, conn, d.Get("name").(string), paramsToModify)

			if err != nil {
				return diag.Errorf("error modifying MemoryDB Parameter Group (%s) parameters: %s", d.Id(), err)
			}
		}
	}

	if d.HasChange("tags_all") {
		o, n := d.GetChange("tags_all")

		if err := UpdateTags(conn, d.Get("arn").(string), o, n); err != nil {
			return diag.Errorf("error updating MemoryDB Parameter Group (%s) tags: %s", d.Id(), err)
		}
	}

	return resourceParameterGroupRead(ctx, d, meta)
}

func resourceParameterGroupRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	conn := meta.(*conns.AWSClient).MemoryDBConn
	defaultTagsConfig := meta.(*conns.AWSClient).DefaultTagsConfig
	ignoreTagsConfig := meta.(*conns.AWSClient).IgnoreTagsConfig

	group, err := FindParameterGroupByName(ctx, conn, d.Id())

	if !d.IsNewResource() && tfresource.NotFound(err) {
		log.Printf("[WARN] MemoryDB Parameter Group (%s) not found, removing from state", d.Id())
		d.SetId("")
		return nil
	}

	if err != nil {
		return diag.Errorf("error reading MemoryDB Parameter Group (%s): %s", d.Id(), err)
	}

	d.Set("arn", group.ARN)
	d.Set("description", group.Description)
	d.Set("family", group.Family)
	d.Set("name", group.Name)
	d.Set("name_prefix", create.NamePrefixFromName(aws.StringValue(group.Name)))

	userDefinedParameters := createUserDefinedParameterMap(d)

	parameters, err := listParameterGroupParameters(ctx, conn, d.Get("family").(string), d.Id(), userDefinedParameters)
	if err != nil {
		return diag.Errorf("error listing parameters for MemoryDB Parameter Group (%s): %s", d.Id(), err)
	}

	if err := d.Set("parameter", flattenParameters(parameters)); err != nil {
		return diag.Errorf("failed to set parameter: %s", err)
	}

	tags, err := ListTags(conn, d.Get("arn").(string))

	if err != nil {
		return diag.Errorf("error listing tags for MemoryDB Parameter Group (%s): %s", d.Id(), err)
	}

	tags = tags.IgnoreAWS().IgnoreConfig(ignoreTagsConfig)

	//lintignore:AWSR002
	if err := d.Set("tags", tags.RemoveDefaultConfig(defaultTagsConfig).Map()); err != nil {
		return diag.Errorf("error setting tags for MemoryDB Parameter Group (%s): %s", d.Id(), err)
	}

	if err := d.Set("tags_all", tags.Map()); err != nil {
		return diag.Errorf("error setting tags_all for MemoryDB Parameter Group (%s): %s", d.Id(), err)
	}

	return nil
}

func resourceParameterGroupDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	conn := meta.(*conns.AWSClient).MemoryDBConn

	log.Printf("[DEBUG] Deleting MemoryDB Parameter Group: (%s)", d.Id())
	_, err := conn.DeleteParameterGroupWithContext(ctx, &memorydb.DeleteParameterGroupInput{
		ParameterGroupName: aws.String(d.Id()),
	})

	if tfawserr.ErrCodeEquals(err, memorydb.ErrCodeParameterGroupNotFoundFault) {
		return nil
	}

	if err != nil {
		return diag.Errorf("error deleting MemoryDB Parameter Group (%s): %s", d.Id(), err)
	}

	return nil
}

// resetParameterGroupParameters resets the given parameters to their default values.
func resetParameterGroupParameters(ctx context.Context, conn *memorydb.MemoryDB, name string, parameters []*memorydb.ParameterNameValue) error {
	var parameterNames []*string
	for _, parameter := range parameters {
		parameterNames = append(parameterNames, parameter.ParameterName)
	}

	input := memorydb.ResetParameterGroupInput{
		ParameterGroupName: aws.String(name),
		ParameterNames:     parameterNames,
	}

	return resource.Retry(30*time.Second, func() *resource.RetryError {
		_, err := conn.ResetParameterGroupWithContext(ctx, &input)
		if err != nil {
			if tfawserr.ErrMessageContains(err, memorydb.ErrCodeInvalidParameterGroupStateFault, " has pending changes") {
				return resource.RetryableError(err)
			}
			return resource.NonRetryableError(err)
		}
		return nil
	})
}

// modifyParameterGroupParameters updates the given parameters.
func modifyParameterGroupParameters(ctx context.Context, conn *memorydb.MemoryDB, name string, parameters []*memorydb.ParameterNameValue) error {
	input := memorydb.UpdateParameterGroupInput{
		ParameterGroupName:  aws.String(name),
		ParameterNameValues: parameters,
	}
	_, err := conn.UpdateParameterGroupWithContext(ctx, &input)
	return err
}

// listParameterGroupParameters returns the user-defined MemoryDB parameters
// in the group with the given name and family.
//
// Parameters given in userDefined will be returned even if the value is equal
// to the default.
func listParameterGroupParameters(ctx context.Context, conn *memorydb.MemoryDB, family, name string, userDefined map[string]string) ([]*memorydb.Parameter, error) {
	query := func(ctx context.Context, parameterGroupName string) ([]*memorydb.Parameter, error) {
		input := memorydb.DescribeParametersInput{
			ParameterGroupName: aws.String(parameterGroupName),
		}

		output, err := conn.DescribeParametersWithContext(ctx, &input)
		if err != nil {
			return nil, err
		}

		return output.Parameters, nil
	}

	// There isn't an official API for defaults, and the mapping of family
	// to default parameter group name is a guess.

	defaultsFamily := "default." + strings.ReplaceAll(family, "_", "-")

	defaults, err := query(ctx, defaultsFamily)
	if err != nil {
		return nil, fmt.Errorf("list defaults for family %s: %w", defaultsFamily, err)
	}

	defaultValueByName := map[string]string{}
	for _, defaultPV := range defaults {
		defaultValueByName[aws.StringValue(defaultPV.Name)] = aws.StringValue(defaultPV.Value)
	}

	current, err := query(ctx, name)
	if err != nil {
		return nil, err
	}

	var result []*memorydb.Parameter

	for _, parameter := range current {
		name := aws.StringValue(parameter.Name)
		currentValue := aws.StringValue(parameter.Value)
		defaultValue := defaultValueByName[name]
		_, isUserDefined := userDefined[name]

		if currentValue != defaultValue || isUserDefined {
			result = append(result, parameter)
		}
	}

	return result, nil
}

// ParameterHash was copy-pasted from ElastiCache.
func ParameterHash(v interface{}) int {
	var buf bytes.Buffer
	m := v.(map[string]interface{})
	buf.WriteString(fmt.Sprintf("%s-", m["name"].(string)))
	buf.WriteString(fmt.Sprintf("%s-", m["value"].(string)))

	return create.StringHashcode(buf.String())
}

// ParameterChanges was copy-pasted from ElastiCache.
func ParameterChanges(o, n interface{}) (remove, addOrUpdate []*memorydb.ParameterNameValue) {
	if o == nil {
		o = new(schema.Set)
	}
	if n == nil {
		n = new(schema.Set)
	}

	os := o.(*schema.Set)
	ns := n.(*schema.Set)

	om := make(map[string]*memorydb.ParameterNameValue, os.Len())
	for _, raw := range os.List() {
		param := raw.(map[string]interface{})
		om[param["name"].(string)] = expandParameterNameValue(param)
	}
	nm := make(map[string]*memorydb.ParameterNameValue, len(addOrUpdate))
	for _, raw := range ns.List() {
		param := raw.(map[string]interface{})
		nm[param["name"].(string)] = expandParameterNameValue(param)
	}

	// Remove: key is in old, but not in new
	remove = make([]*memorydb.ParameterNameValue, 0, os.Len())
	for k := range om {
		if _, ok := nm[k]; !ok {
			remove = append(remove, om[k])
		}
	}

	// Add or Update: key is in new, but not in old or has changed value
	addOrUpdate = make([]*memorydb.ParameterNameValue, 0, ns.Len())
	for k, nv := range nm {
		ov, ok := om[k]
		if !ok || ok && (aws.StringValue(nv.ParameterValue) != aws.StringValue(ov.ParameterValue)) {
			addOrUpdate = append(addOrUpdate, nm[k])
		}
	}

	return remove, addOrUpdate
}

func flattenParameters(list []*memorydb.Parameter) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(list))
	for _, i := range list {
		if i.Value != nil {
			result = append(result, map[string]interface{}{
				"name":  strings.ToLower(aws.StringValue(i.Name)),
				"value": aws.StringValue(i.Value),
			})
		}
	}
	return result
}

func expandParameterNameValue(param map[string]interface{}) *memorydb.ParameterNameValue {
	return &memorydb.ParameterNameValue{
		ParameterName:  aws.String(param["name"].(string)),
		ParameterValue: aws.String(param["value"].(string)),
	}
}

func createUserDefinedParameterMap(d *schema.ResourceData) map[string]string {
	result := map[string]string{}

	for _, param := range d.Get("parameter").(*schema.Set).List() {
		m, ok := param.(map[string]interface{})
		if !ok {
			continue
		}

		name, ok := m["name"].(string)
		if !ok || name == "" {
			continue
		}

		value, ok := m["value"].(string)
		if !ok || value == "" {
			continue
		}

		result[name] = value
	}

	return result
}
