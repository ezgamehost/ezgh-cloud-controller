package computeapi

import (
	"context"
	"net/http"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

// TerminateMember removes instance from group, draining it first through the group's
// kubernetesOwner. With decrementDesired the group's desiredCapacity drops by one instead of a
// replacement launching. 200 with the member (state retiring); 409 below_min_size; 404 when
// instance isn't a member. Checked as compute.autoscalingGroups.update.
//
// shortcut: compute's route hasn't landed, so this is built against the design's
// shape and isn't in compute-api.json's contract test; add it there when ezgh-cli gains the method.
func (c *Client) TerminateMember(ctx context.Context, orgID, projectID, group, instance string, decrementDesired bool) (*AutoscalingMember, error) {
	var out AutoscalingMember
	body := struct {
		DecrementDesired bool `json:"decrementDesired"`
	}{decrementDesired}
	_, err := c.API.Do(ctx, apiclient.Request{
		Method: http.MethodPost, Path: projectPath(orgID, projectID, "/autoscaling-groups/{}/members/{}/terminate", group, instance),
		Body: body, Create: true,
	}, &out)
	return &out, err
}
