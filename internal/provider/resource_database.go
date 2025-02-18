package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"net/http"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/joeandaverde/astra-client-go/v2/astra"
)

var availableCloudProviders = []string{
	"aws",
	"gcp",
}

var availableTiers = []string{
	"serverless",
}

var databaseCreateTimeout = time.Minute * 20
var databaseReadTimeout = time.Minute * 5

func resourceDatabase() *schema.Resource {
	return &schema.Resource{
		Description: "Astra Database.",

		CreateContext: resourceDatabaseCreate,
		ReadContext:   resourceDatabaseRead,
		DeleteContext: resourceDatabaseDelete,

		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		Timeouts: &schema.ResourceTimeout{
			Create: &databaseCreateTimeout,
			Read:   &databaseReadTimeout,
		},

		Schema: map[string]*schema.Schema{
			// Required
			"name": {
				Description:  "Astra database name.",
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringMatch(regexp.MustCompile("^.{2,}"), "name must be atleast 2 characters"),
			},
			"keyspace": {
				Description:      "keyspace",
				Type:             schema.TypeString,
				Required:         true,
				ForceNew:         true,
				ValidateDiagFunc: validateKeyspace,
			},
			"cloud_provider": {
				Description:      "The cloud provider to launch the database.",
				Type:             schema.TypeString,
				Required:         true,
				ForceNew:         true,
				ValidateFunc:     validation.StringInSlice(availableCloudProviders, true),
				DiffSuppressFunc: ignoreCase,
			},
			"region": {
				Description: "Astra database id.",
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
			},

			// Computed
			"owner_id": {
				Description: "The owner id.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"organization_id": {
				Description: "The org id.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"status": {
				Description: "The status",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"cqlsh_url": {
				Description: "The cqlsh_url",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"grafana_url": {
				Description: "The grafana_url",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"data_endpoint_url": {
				Description: "The data_endpoint_url",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"graphql_url": {
				Description: "The graphql_url",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"node_count": {
				Description: "The node_count",
				Type:        schema.TypeInt,
				Computed:    true,
			},
			"replication_factor": {
				Description: "The replication_factor",
				Type:        schema.TypeInt,
				Computed:    true,
			},
			"total_storage": {
				Description: "The total_storage",
				Type:        schema.TypeInt,
				Computed:    true,
			},
			"additional_keyspaces": {
				Description: "The total_storage",
				Type:        schema.TypeList,
				Computed:    true,
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
			},
		},
	}
}

func resourceDatabaseCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*astra.ClientWithResponses)

	name := d.Get("name").(string)
	keyspace := d.Get("keyspace").(string)
	cloudProvider := d.Get("cloud_provider").(string)
	region := d.Get("region").(string)

	regionsResp, err := client.ListAvailableRegionsWithResponse(ctx)
	if err != nil {
		return diag.FromErr(err)
	} else if regionsResp.StatusCode() != 200 {
		return diag.Errorf("unexpected list available regions response: %s", string(regionsResp.Body))
	}

	availableRegions := astra.AvailableRegionCombinationSlice(regionsResp.JSON200)
	dbRegion := findMatchingRegion(cloudProvider, region, "serverless", availableRegions)
	if dbRegion == nil {
		return diag.Errorf("cloud provider and region combination not available: %s/%s", cloudProvider, region)
	}

	resp, err := client.CreateDatabaseWithResponse(ctx, astra.CreateDatabaseJSONRequestBody{
		Name:          name,
		Keyspace:      keyspace,
		CloudProvider: dbRegion.CloudProvider,
		CapacityUnits: 1,
		Region:        dbRegion.Region,
		Tier:          dbRegion.Tier,
	})
	if err != nil {
		return diag.FromErr(err)
	}
	if resp.StatusCode() != http.StatusCreated {
		return diag.Errorf("unexpected create database response: %s", string(resp.Body))
	}

	databaseID := resp.HTTPResponse.Header.Get("location")

	// Wait for the database to be ACTIVE then set resource data
	if err := resource.RetryContext(ctx, d.Timeout(schema.TimeoutCreate), func() *resource.RetryError {
		res, err := client.GetDatabaseWithResponse(ctx, astra.DatabaseIdParam(databaseID))
		// Errors sending request should be retried and are assumed to be transient
		if err != nil {
			return resource.RetryableError(err)
		}

		// Status code >=5xx are assumed to be transient
		if res.StatusCode() >= 500 {
			return resource.RetryableError(fmt.Errorf("error while fetching databae: %s", string(res.Body)))
		}

		// Status code > 200 NOT retried
		if res.StatusCode() > 200 || res.JSON200 == nil {
			return resource.NonRetryableError(fmt.Errorf("unexpected response fetching database: %s", string(res.Body)))
		}

		// Success fetching database
		db := res.JSON200
		switch db.Status {
		case astra.StatusEnum_ERROR, astra.StatusEnum_TERMINATED, astra.StatusEnum_TERMINATING:
			// If the database reached a terminal state it will never become active
			return resource.NonRetryableError(fmt.Errorf("database failed to reach active status: status=%s", db.Status))
		case astra.StatusEnum_ACTIVE:
			if err := setDatabaseResourceData(d, db); err != nil {
				return resource.NonRetryableError(err)
			}
			return nil
		default:
			return resource.RetryableError(fmt.Errorf("expected database to be active but was %s", db.Status))
		}
	}); err != nil {
		return diag.FromErr(err)
	}

	return nil
}

func resourceDatabaseRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*astra.ClientWithResponses)

	databaseID := d.Id()

	if err := resource.RetryContext(ctx, d.Timeout(schema.TimeoutRead), func() *resource.RetryError {
		resp, err := client.GetDatabaseWithResponse(ctx, astra.DatabaseIdParam(databaseID))
		if err != nil {
			return resource.RetryableError(fmt.Errorf("unable to fetch database (%s): %v", databaseID, err))
		}

		// Remove from state when database not found
		if resp.JSON404 != nil || resp.StatusCode() == http.StatusNotFound {
			d.SetId("")
			return nil
		}

		// Retry on 5XX errors
		if resp.StatusCode() >= 500 {
			return resource.RetryableError(fmt.Errorf("error fetching database (%s): %v", databaseID, err))
		}

		// Don't retry for non 200 status code
		db := resp.JSON200
		if db == nil {
			return resource.NonRetryableError(fmt.Errorf("unexpected response fetching database (%s): %s", databaseID, string(resp.Body)))
		}

		// If the database is TERMINATING or TERMINATED then remove it from the state
		if db.Status == astra.StatusEnum_TERMINATING || db.Status == astra.StatusEnum_TERMINATED {
			d.SetId("")
			return nil
		}

		// Add the database to state
		if err := setDatabaseResourceData(d, db); err != nil {
			return resource.NonRetryableError(err)
		}

		return nil
	}); err != nil {
		return diag.FromErr(err)
	}

	return nil
}

func resourceDatabaseDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*astra.ClientWithResponses)

	databaseID := d.Id()

	if err := resource.RetryContext(ctx, d.Timeout(schema.TimeoutDelete), func() *resource.RetryError {
		resp, err := client.TerminateDatabaseWithResponse(ctx, astra.DatabaseIdParam(databaseID), &astra.TerminateDatabaseParams{})
		if err != nil {
			return resource.RetryableError(err)
		}

		// Status code 5XX are considered transient
		if resp.StatusCode() >= 500 {
			return resource.RetryableError(fmt.Errorf("error terminating database: %s", string(resp.Body)))
		}

		// Status code 4XX are NOT retried
		if resp.StatusCode() >= 400 {
			return resource.NonRetryableError(fmt.Errorf("unexpected response attempting to terminate database: %s", string(resp.Body)))
		}

		return nil
	}); err != nil {
		return diag.FromErr(err)
	}

	d.SetId("")
	return nil
}

func setDatabaseResourceData(d *schema.ResourceData, db *astra.Database) error {
	d.SetId(db.Id)
	flatDb := flattenDatabase(db)
	for k, v := range flatDb {
		if k == "id" {
			continue
		}
		if err := d.Set(k, v); err != nil {
			return err
		}
	}
	return nil
}

func flattenDatabase(db *astra.Database) map[string]interface{} {
	return map[string]interface{}{
		"id":                   db.Id,
		"name":                 astra.StringValue(db.Info.Name),
		"organization_id":      db.OrgId,
		"owner_id":             db.OwnerId,
		"status":               string(db.Status),
		"grafana_url":          astra.StringValue(db.GrafanaUrl),
		"graphql_url":          astra.StringValue(db.GraphqlUrl),
		"data_endpoint_url":    astra.StringValue(db.DataEndpointUrl),
		"cqlsh_url":            astra.StringValue(db.CqlshUrl),
		"cloud_provider":       astra.StringValue(db.Info.CloudProvider),
		"region":               astra.StringValue(db.Info.Region),
		"keyspace":             astra.StringValue(db.Info.Keyspace),
		"additional_keyspaces": astra.StringSlice(db.Info.AdditionalKeyspaces),
	}
}

func findMatchingRegion(provider, region, tier string, availableRegions []astra.AvailableRegionCombination) *astra.AvailableRegionCombination {
	for _, ar := range availableRegions {
		if strings.EqualFold(ar.Tier, tier) &&
			strings.EqualFold(ar.CloudProvider, provider) &&
			strings.EqualFold(ar.Region, region) {
			return &ar
		}
	}

	return nil
}
