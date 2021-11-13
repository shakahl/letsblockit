package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgtype"
	"github.com/labstack/echo/v4"
	"github.com/xvello/letsblockit/src/db"
	"github.com/xvello/letsblockit/src/filters"
)

func (s *Server) listFilters(c echo.Context) error {
	tag := c.Param("tag")
	hc := s.buildPageContext(c, "Available uBlock filter templates")
	if tag != "" {
		hc.Title = "Available filter templates for " + tag
		hc.Add("tag_search", tag)
	}

	hc.Add("filter_tags", s.filters.GetTags())
	var activeNames map[string]struct{}
	if hc.UserVerified {
		activeNames = make(map[string]struct{})
		names, _ := s.store.GetActiveFiltersForUser(c.Request().Context(), hc.UserID)
		for _, n := range names {
			activeNames[n] = struct{}{}
		}
	}

	// Filter and group filters, or quick return on homepage
	if len(activeNames) == 0 && len(tag) == 0 {
		hc.Add("available_filters", s.filters.GetFilters())
	} else {
		var active, available []*filters.Filter
		for _, f := range s.filters.GetFilters() {
			if tag != "" {
				matching := false
				for _, t := range f.Tags {
					if t == tag {
						matching = true
						break
					}
				}
				if !matching {
					continue
				}
			}
			if _, ok := activeNames[f.Name]; ok {
				active = append(active, f)
			} else {
				available = append(available, f)
			}
		}
		hc.Add("active_filters", active)
		hc.Add("available_filters", available)
	}
	return s.pages.Render(c, "list-filters", hc)
}

func (s *Server) viewFilter(c echo.Context) error {
	filter, err := s.filters.GetFilter(c.Param("name"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	hc := s.buildPageContext(c, fmt.Sprintf("How to %s with uBlock or Adblock", lowerFirst(filter.Title)))
	hc.Add("filter", filter)

	// Parse filters param and render output if non-empty
	params, save, disable, err := parseFilterParams(c, filter)
	if err != nil {
		return err
	}

	// Save filter params if requested
	if save && hc.UserVerified {
		var out pgtype.JSONB
		if err = out.Set(params); err != nil {
			return err
		}
		if err = s.upsertFilterParams(c, hc.UserID, filter.Name, params); err != nil {
			return err
		}
		hc.Add("saved_ok", true)
		hc.Add("has_instance", true)
	}

	// Handle deletion if requested, remove all instances matching a given name
	if disable && hc.UserVerified {
		if err = s.store.DeleteInstanceForUserAndFilter(c.Request().Context(), db.DeleteInstanceForUserAndFilterParams{
			UserID:     hc.UserID,
			FilterName: filter.Name,
		}); err != nil {
			return err
		}
		return s.redirect(c, "list-filters")
	}

	// If no params are passed, source from the user's filters
	if !save && params == nil && hc.UserVerified {
		f, err := s.store.GetInstanceForUserAndFilter(c.Request().Context(), db.GetInstanceForUserAndFilterParams{
			UserID:     hc.UserID,
			FilterName: filter.Name,
		})
		switch err {
		case nil:
			if err = f.AssignTo(&params); err != nil {
				return err
			}
			hc.Add("has_instance", true)
		case db.NotFound: // ok
		default:
			return err
		}
	}

	// If no config found, inject the default ones
	if params == nil {
		params = make(map[string]interface{})
		for _, p := range filter.Params {
			params[p.Name] = p.Default
		}
	}

	// Render the filter template
	var buf strings.Builder
	if err = s.filters.Render(&buf, filter.Name, params); err != nil {
		return err
	}
	hc.Add("rendered", buf.String())
	hc.Add("params", params)

	return s.pages.Render(c, "view-filter", hc)
}

func (s *Server) viewFilterRender(c echo.Context) error {
	filter, err := s.filters.GetFilter(c.Param("name"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	// Parse filters param and render output if non empty
	params, _, _, err := parseFilterParams(c, filter)
	if err != nil {
		return err
	}

	// If no params are passed, inject the default ones
	if params == nil {
		params = make(map[string]interface{})
		for _, p := range filter.Params {
			params[p.Name] = p.Default
		}
	}

	// Render the filter template
	var buf strings.Builder
	if err = s.filters.Render(&buf, filter.Name, params); err != nil {
		return err
	}
	hc := s.buildPageContext(c, "")
	hc.NakedContent = true
	hc.Add("rendered", buf.String())

	return s.pages.Render(c, "view-filter-render", hc)
}

func (s *Server) upsertFilterParams(c echo.Context, user uuid.UUID, filter string, params map[string]interface{}) error {
	var out pgtype.JSONB
	if err := out.Set(&params); err != nil {
		return err
	}
	return s.store.RunTx(c, func(ctx context.Context, q db.Querier) error {
		count, err := q.CountInstanceForUserAndFilter(ctx, db.CountInstanceForUserAndFilterParams{
			UserID:     user,
			FilterName: filter,
		})
		if err != nil {
			return err
		}
		if count == 0 {
			return q.CreateInstanceForUserAndFilter(ctx, db.CreateInstanceForUserAndFilterParams{
				UserID:     user,
				FilterName: filter,
				Params:     out,
			})
		} else {
			return q.UpdateInstanceForUserAndFilter(ctx, db.UpdateInstanceForUserAndFilterParams{
				UserID:     user,
				FilterName: filter,
				Params:     out,
			})
		}
	})
}

func parseFilterParams(c echo.Context, filter *filters.Filter) (map[string]interface{}, bool, bool, error) {
	formParams, err := c.FormParams()
	if err != nil {
		return nil, false, false, err
	}
	if len(formParams) == 0 {
		return nil, false, false, nil
	}

	_, save := formParams["__save"]
	_, disable := formParams["__disable"]
	params := make(map[string]interface{})
	for _, p := range filter.Params {
		switch p.Type {
		case filters.StringListParam:
			var values []string
			for _, v := range formParams[p.Name] {
				if v != "" {
					values = append(values, v)
				}
			}
			params[p.Name] = values
		case filters.StringParam:
			params[p.Name] = formParams.Get(p.Name)
		case filters.MultiLineParam:
			params[p.Name] = formParams.Get(p.Name)
		case filters.BooleanParam:
			params[p.Name] = formParams.Get(p.Name) == "on"
		default:
			return nil, false, false, echo.NewHTTPError(http.StatusInternalServerError, "unknown param type "+p.Type)
		}
	}
	return params, save, disable, err
}

func lowerFirst(s string) string {
	if s == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(r)) + s[n:]
}
