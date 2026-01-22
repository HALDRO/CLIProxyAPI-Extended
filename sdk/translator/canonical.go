package translator

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

// CanonicalAdapter is an optional hook that lets the SDK translator delegate
// request/response translation to an alternative implementation.
//
// This exists to allow wiring internal translator implementations (e.g. translator_new)
// from the application entrypoint without sdk/translator importing internal/ packages.
type CanonicalAdapter interface {
	TranslateRequest(ctx context.Context, from, to Format, model string, rawJSON []byte, stream bool) ([]byte, error)
	TranslateNonStream(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) (string, error)
	TranslateStream(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) ([]string, error)
}

var canonicalEnabled atomic.Bool
var canonicalAdapter atomic.Value // stores CanonicalAdapter

func EnableCanonicalTranslator(enabled bool) {
	canonicalEnabled.Store(enabled)
}

func SetCanonicalAdapter(adapter CanonicalAdapter) {
	canonicalAdapter.Store(adapter)
}

func getCanonicalAdapter() (CanonicalAdapter, bool) {
	v := canonicalAdapter.Load()
	if v == nil {
		return nil, false
	}
	ad, ok := v.(CanonicalAdapter)
	return ad, ok && ad != nil
}

var errCanonicalNotConfigured = errors.New("canonical translator enabled but no adapter is configured")

// TranslateRequestE is like TranslateRequest but returns an error.
// When canonical mode is enabled, it requires a configured CanonicalAdapter and never falls back.
func TranslateRequestE(ctx context.Context, from, to Format, model string, rawJSON []byte, stream bool) ([]byte, error) {
	if canonicalEnabled.Load() {
		ad, ok := getCanonicalAdapter()
		if !ok {
			return nil, errCanonicalNotConfigured
		}
		return ad.TranslateRequest(ctx, from, to, model, rawJSON, stream)
	}
	return TranslateRequest(from, to, model, rawJSON, stream), nil
}

// TranslateNonStreamE is like TranslateNonStream but returns an error.
// When canonical mode is enabled, it requires a configured CanonicalAdapter and never falls back.
func TranslateNonStreamE(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) (string, error) {
	if canonicalEnabled.Load() {
		ad, ok := getCanonicalAdapter()
		if !ok {
			return "", errCanonicalNotConfigured
		}
		return ad.TranslateNonStream(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, rawJSON, param)
	}
	return TranslateNonStream(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, rawJSON, param), nil
}

// TranslateStreamE is like TranslateStream but returns an error.
// When canonical mode is enabled, it requires a configured CanonicalAdapter and never falls back.
func TranslateStreamE(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) ([]string, error) {
	if canonicalEnabled.Load() {
		ad, ok := getCanonicalAdapter()
		if !ok {
			return nil, errCanonicalNotConfigured
		}
		return ad.TranslateStream(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, rawJSON, param)
	}
	return TranslateStream(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, rawJSON, param), nil
}

// TranslateRequestPairE translates both the effective request payload and the "original" payload
// (used for payload-config comparisons) using the same translation backend.
//
// When canonical mode is enabled, this will never fall back and will return an error on failure.
func TranslateRequestPairE(
	ctx context.Context,
	from, to Format,
	model string,
	payload []byte,
	originalPayload []byte,
	stream bool,
) (originalTranslated []byte, translated []byte, err error) {
	if originalPayload == nil {
		originalPayload = payload
	}

	originalTranslated, err = TranslateRequestE(ctx, from, to, model, originalPayload, stream)
	if err != nil {
		return nil, nil, fmt.Errorf("translate request (original): %w", err)
	}

	translated, err = TranslateRequestE(ctx, from, to, model, payload, stream)
	if err != nil {
		return nil, nil, fmt.Errorf("translate request: %w", err)
	}

	return originalTranslated, translated, nil
}
