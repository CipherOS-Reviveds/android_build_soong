// Copyright 2015 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package android

import (
	"reflect"

	"github.com/google/blueprint"
	"github.com/google/blueprint/proptools"
)

// Phases:
//   run Pre-arch mutators
//   run archMutator
//   run Pre-deps mutators
//   run depsMutator
//   run PostDeps mutators
//   run FinalDeps mutators (CreateVariations disallowed in this phase)
//   continue on to GenerateAndroidBuildActions

func registerMutatorsToContext(ctx *blueprint.Context, mutators []*mutator) {
	for _, t := range mutators {
		var handle blueprint.MutatorHandle
		if t.bottomUpMutator != nil {
			handle = ctx.RegisterBottomUpMutator(t.name, t.bottomUpMutator)
		} else if t.topDownMutator != nil {
			handle = ctx.RegisterTopDownMutator(t.name, t.topDownMutator)
		}
		if t.parallel {
			handle.Parallel()
		}
	}
}

func registerMutators(ctx *blueprint.Context, preArch, preDeps, postDeps, finalDeps []RegisterMutatorFunc) {
	mctx := &registerMutatorsContext{}

	register := func(funcs []RegisterMutatorFunc) {
		for _, f := range funcs {
			f(mctx)
		}
	}

	register(preArch)

	register(preDeps)

	mctx.BottomUp("deps", depsMutator).Parallel()

	register(postDeps)

	mctx.finalPhase = true
	register(finalDeps)

	registerMutatorsToContext(ctx, mctx.mutators)
}

type registerMutatorsContext struct {
	mutators   []*mutator
	finalPhase bool
}

type RegisterMutatorsContext interface {
	TopDown(name string, m TopDownMutator) MutatorHandle
	BottomUp(name string, m BottomUpMutator) MutatorHandle
	BottomUpBlueprint(name string, m blueprint.BottomUpMutator) MutatorHandle
}

type RegisterMutatorFunc func(RegisterMutatorsContext)

var preArch = []RegisterMutatorFunc{
	RegisterNamespaceMutator,

	// Check the visibility rules are valid.
	//
	// This must run after the package renamer mutators so that any issues found during
	// validation of the package's default_visibility property are reported using the
	// correct package name and not the synthetic name.
	//
	// This must also be run before defaults mutators as the rules for validation are
	// different before checking the rules than they are afterwards. e.g.
	//    visibility: ["//visibility:private", "//visibility:public"]
	// would be invalid if specified in a module definition but is valid if it results
	// from something like this:
	//
	//    defaults {
	//        name: "defaults",
	//        // Be inaccessible outside a package by default.
	//        visibility: ["//visibility:private"]
	//    }
	//
	//    defaultable_module {
	//        name: "defaultable_module",
	//        defaults: ["defaults"],
	//        // Override the default.
	//        visibility: ["//visibility:public"]
	//    }
	//
	RegisterVisibilityRuleChecker,

	// Apply properties from defaults modules to the referencing modules.
	//
	// Any mutators that are added before this will not see any modules created by
	// a DefaultableHook.
	RegisterDefaultsPreArchMutators,

	// Add dependencies on any components so that any component references can be
	// resolved within the deps mutator.
	//
	// Must be run after defaults so it can be used to create dependencies on the
	// component modules that are creating in a DefaultableHook.
	//
	// Must be run before RegisterPrebuiltsPreArchMutators, i.e. before prebuilts are
	// renamed. That is so that if a module creates components using a prebuilt module
	// type that any dependencies (which must use prebuilt_ prefixes) are resolved to
	// the prebuilt module and not the source module.
	RegisterComponentsMutator,

	// Create an association between prebuilt modules and their corresponding source
	// modules (if any).
	//
	// Must be run after defaults mutators to ensure that any modules created by
	// a DefaultableHook can be either a prebuilt or a source module with a matching
	// prebuilt.
	RegisterPrebuiltsPreArchMutators,

	// Gather the visibility rules for all modules for us during visibility enforcement.
	//
	// This must come after the defaults mutators to ensure that any visibility supplied
	// in a defaults module has been successfully applied before the rules are gathered.
	RegisterVisibilityRuleGatherer,
}

func registerArchMutator(ctx RegisterMutatorsContext) {
	ctx.BottomUpBlueprint("os", osMutator).Parallel()
	ctx.BottomUp("image", imageMutator).Parallel()
	ctx.BottomUpBlueprint("arch", archMutator).Parallel()
}

var preDeps = []RegisterMutatorFunc{
	registerArchMutator,
}

var postDeps = []RegisterMutatorFunc{
	registerPathDepsMutator,
	RegisterPrebuiltsPostDepsMutators,
	RegisterVisibilityRuleEnforcer,
	RegisterNeverallowMutator,
	RegisterOverridePostDepsMutators,
}

var finalDeps = []RegisterMutatorFunc{}

func PreArchMutators(f RegisterMutatorFunc) {
	preArch = append(preArch, f)
}

func PreDepsMutators(f RegisterMutatorFunc) {
	preDeps = append(preDeps, f)
}

func PostDepsMutators(f RegisterMutatorFunc) {
	postDeps = append(postDeps, f)
}

func FinalDepsMutators(f RegisterMutatorFunc) {
	finalDeps = append(finalDeps, f)
}

type BaseMutatorContext interface {
	BaseModuleContext

	// MutatorName returns the name that this mutator was registered with.
	MutatorName() string

	// Rename all variants of a module.  The new name is not visible to calls to ModuleName,
	// AddDependency or OtherModuleName until after this mutator pass is complete.
	Rename(name string)
}

type TopDownMutator func(TopDownMutatorContext)

type TopDownMutatorContext interface {
	BaseMutatorContext

	// CreateModule creates a new module by calling the factory method for the specified moduleType, and applies
	// the specified property structs to it as if the properties were set in a blueprint file.
	CreateModule(ModuleFactory, ...interface{}) Module
}

type topDownMutatorContext struct {
	bp blueprint.TopDownMutatorContext
	baseModuleContext
}

type BottomUpMutator func(BottomUpMutatorContext)

type BottomUpMutatorContext interface {
	BaseMutatorContext

	// AddDependency adds a dependency to the given module.
	// Does not affect the ordering of the current mutator pass, but will be ordered
	// correctly for all future mutator passes.
	AddDependency(module blueprint.Module, tag blueprint.DependencyTag, name ...string)

	// AddReverseDependency adds a dependency from the destination to the given module.
	// Does not affect the ordering of the current mutator pass, but will be ordered
	// correctly for all future mutator passes.  All reverse dependencies for a destination module are
	// collected until the end of the mutator pass, sorted by name, and then appended to the destination
	// module's dependency list.
	AddReverseDependency(module blueprint.Module, tag blueprint.DependencyTag, name string)

	// CreateVariations splits  a module into multiple variants, one for each name in the variationNames
	// parameter.  It returns a list of new modules in the same order as the variationNames
	// list.
	//
	// If any of the dependencies of the module being operated on were already split
	// by calling CreateVariations with the same name, the dependency will automatically
	// be updated to point the matching variant.
	//
	// If a module is split, and then a module depending on the first module is not split
	// when the Mutator is later called on it, the dependency of the depending module will
	// automatically be updated to point to the first variant.
	CreateVariations(...string) []Module

	// CreateLocationVariations splits a module into multiple variants, one for each name in the variantNames
	// parameter.  It returns a list of new modules in the same order as the variantNames
	// list.
	//
	// Local variations do not affect automatic dependency resolution - dependencies added
	// to the split module via deps or DynamicDependerModule must exactly match a variant
	// that contains all the non-local variations.
	CreateLocalVariations(...string) []Module

	// SetDependencyVariation sets all dangling dependencies on the current module to point to the variation
	// with given name. This function ignores the default variation set by SetDefaultDependencyVariation.
	SetDependencyVariation(string)

	// SetDefaultDependencyVariation sets the default variation when a dangling reference is detected
	// during the subsequent calls on Create*Variations* functions. To reset, set it to nil.
	SetDefaultDependencyVariation(*string)

	// AddVariationDependencies adds deps as dependencies of the current module, but uses the variations
	// argument to select which variant of the dependency to use.  A variant of the dependency must
	// exist that matches the all of the non-local variations of the current module, plus the variations
	// argument.
	AddVariationDependencies([]blueprint.Variation, blueprint.DependencyTag, ...string)

	// AddFarVariationDependencies adds deps as dependencies of the current module, but uses the
	// variations argument to select which variant of the dependency to use.  A variant of the
	// dependency must exist that matches the variations argument, but may also have other variations.
	// For any unspecified variation the first variant will be used.
	//
	// Unlike AddVariationDependencies, the variations of the current module are ignored - the
	// dependency only needs to match the supplied variations.
	AddFarVariationDependencies([]blueprint.Variation, blueprint.DependencyTag, ...string)

	// AddInterVariantDependency adds a dependency between two variants of the same module.  Variants are always
	// ordered in the same orderas they were listed in CreateVariations, and AddInterVariantDependency does not change
	// that ordering, but it associates a DependencyTag with the dependency and makes it visible to VisitDirectDeps,
	// WalkDeps, etc.
	AddInterVariantDependency(tag blueprint.DependencyTag, from, to blueprint.Module)

	// ReplaceDependencies replaces all dependencies on the identical variant of the module with the
	// specified name with the current variant of this module.  Replacements don't take effect until
	// after the mutator pass is finished.
	ReplaceDependencies(string)

	// ReplaceDependencies replaces all dependencies on the identical variant of the module with the
	// specified name with the current variant of this module as long as the supplied predicate returns
	// true.
	//
	// Replacements don't take effect until after the mutator pass is finished.
	ReplaceDependenciesIf(string, blueprint.ReplaceDependencyPredicate)

	// AliasVariation takes a variationName that was passed to CreateVariations for this module,
	// and creates an alias from the current variant (before the mutator has run) to the new
	// variant.  The alias will be valid until the next time a mutator calls CreateVariations or
	// CreateLocalVariations on this module without also calling AliasVariation.  The alias can
	// be used to add dependencies on the newly created variant using the variant map from
	// before CreateVariations was run.
	AliasVariation(variationName string)

	// CreateAliasVariation takes a toVariationName that was passed to CreateVariations for this
	// module, and creates an alias from a new fromVariationName variant the toVariationName
	// variant.  The alias will be valid until the next time a mutator calls CreateVariations or
	// CreateLocalVariations on this module without also calling AliasVariation.  The alias can
	// be used to add dependencies on the toVariationName variant using the fromVariationName
	// variant.
	CreateAliasVariation(fromVariationName, toVariationName string)
}

type bottomUpMutatorContext struct {
	bp blueprint.BottomUpMutatorContext
	baseModuleContext
	finalPhase bool
}

func bottomUpMutatorContextFactory(ctx blueprint.BottomUpMutatorContext, a Module,
	finalPhase bool) BottomUpMutatorContext {

	return &bottomUpMutatorContext{
		bp:                ctx,
		baseModuleContext: a.base().baseModuleContextFactory(ctx),
		finalPhase:        finalPhase,
	}
}

func (x *registerMutatorsContext) BottomUp(name string, m BottomUpMutator) MutatorHandle {
	finalPhase := x.finalPhase
	f := func(ctx blueprint.BottomUpMutatorContext) {
		if a, ok := ctx.Module().(Module); ok {
			m(bottomUpMutatorContextFactory(ctx, a, finalPhase))
		}
	}
	mutator := &mutator{name: name, bottomUpMutator: f}
	x.mutators = append(x.mutators, mutator)
	return mutator
}

func (x *registerMutatorsContext) BottomUpBlueprint(name string, m blueprint.BottomUpMutator) MutatorHandle {
	mutator := &mutator{name: name, bottomUpMutator: m}
	x.mutators = append(x.mutators, mutator)
	return mutator
}

func (x *registerMutatorsContext) TopDown(name string, m TopDownMutator) MutatorHandle {
	f := func(ctx blueprint.TopDownMutatorContext) {
		if a, ok := ctx.Module().(Module); ok {
			actx := &topDownMutatorContext{
				bp:                ctx,
				baseModuleContext: a.base().baseModuleContextFactory(ctx),
			}
			m(actx)
		}
	}
	mutator := &mutator{name: name, topDownMutator: f}
	x.mutators = append(x.mutators, mutator)
	return mutator
}

type MutatorHandle interface {
	Parallel() MutatorHandle
}

func (mutator *mutator) Parallel() MutatorHandle {
	mutator.parallel = true
	return mutator
}

func RegisterComponentsMutator(ctx RegisterMutatorsContext) {
	ctx.BottomUp("component-deps", componentDepsMutator).Parallel()
}

// A special mutator that runs just prior to the deps mutator to allow the dependencies
// on component modules to be added so that they can depend directly on a prebuilt
// module.
func componentDepsMutator(ctx BottomUpMutatorContext) {
	if m := ctx.Module(); m.Enabled() {
		m.ComponentDepsMutator(ctx)
	}
}

func depsMutator(ctx BottomUpMutatorContext) {
	if m := ctx.Module(); m.Enabled() {
		m.DepsMutator(ctx)
	}
}

func (t *topDownMutatorContext) AppendProperties(props ...interface{}) {
	for _, p := range props {
		err := proptools.AppendMatchingProperties(t.Module().base().customizableProperties,
			p, nil)
		if err != nil {
			if propertyErr, ok := err.(*proptools.ExtendPropertyError); ok {
				t.PropertyErrorf(propertyErr.Property, "%s", propertyErr.Err.Error())
			} else {
				panic(err)
			}
		}
	}
}

func (t *topDownMutatorContext) PrependProperties(props ...interface{}) {
	for _, p := range props {
		err := proptools.PrependMatchingProperties(t.Module().base().customizableProperties,
			p, nil)
		if err != nil {
			if propertyErr, ok := err.(*proptools.ExtendPropertyError); ok {
				t.PropertyErrorf(propertyErr.Property, "%s", propertyErr.Err.Error())
			} else {
				panic(err)
			}
		}
	}
}

// android.topDownMutatorContext either has to embed blueprint.TopDownMutatorContext, in which case every method that
// has an overridden version in android.BaseModuleContext has to be manually forwarded to BaseModuleContext to avoid
// ambiguous method errors, or it has to store a blueprint.TopDownMutatorContext non-embedded, in which case every
// non-overridden method has to be forwarded.  There are fewer non-overridden methods, so use the latter.  The following
// methods forward to the identical blueprint versions for topDownMutatorContext and bottomUpMutatorContext.

func (t *topDownMutatorContext) MutatorName() string {
	return t.bp.MutatorName()
}

func (t *topDownMutatorContext) Rename(name string) {
	t.bp.Rename(name)
	t.Module().base().commonProperties.DebugName = name
}

func (t *topDownMutatorContext) CreateModule(factory ModuleFactory, props ...interface{}) Module {
	inherited := []interface{}{&t.Module().base().commonProperties}
	module := t.bp.CreateModule(ModuleFactoryAdaptor(factory), append(inherited, props...)...).(Module)

	if t.Module().base().variableProperties != nil && module.base().variableProperties != nil {
		src := t.Module().base().variableProperties
		dst := []interface{}{
			module.base().variableProperties,
			// Put an empty copy of the src properties into dst so that properties in src that are not in dst
			// don't cause a "failed to find property to extend" error.
			proptools.CloneEmptyProperties(reflect.ValueOf(src)).Interface(),
		}
		err := proptools.AppendMatchingProperties(dst, src, nil)
		if err != nil {
			panic(err)
		}
	}

	return module
}

func (b *bottomUpMutatorContext) MutatorName() string {
	return b.bp.MutatorName()
}

func (b *bottomUpMutatorContext) Rename(name string) {
	b.bp.Rename(name)
	b.Module().base().commonProperties.DebugName = name
}

func (b *bottomUpMutatorContext) AddDependency(module blueprint.Module, tag blueprint.DependencyTag, name ...string) {
	b.bp.AddDependency(module, tag, name...)
}

func (b *bottomUpMutatorContext) AddReverseDependency(module blueprint.Module, tag blueprint.DependencyTag, name string) {
	b.bp.AddReverseDependency(module, tag, name)
}

func (b *bottomUpMutatorContext) CreateVariations(variations ...string) []Module {
	if b.finalPhase {
		panic("CreateVariations not allowed in FinalDepsMutators")
	}

	modules := b.bp.CreateVariations(variations...)

	aModules := make([]Module, len(modules))
	for i := range variations {
		aModules[i] = modules[i].(Module)
		base := aModules[i].base()
		base.commonProperties.DebugMutators = append(base.commonProperties.DebugMutators, b.MutatorName())
		base.commonProperties.DebugVariations = append(base.commonProperties.DebugVariations, variations[i])
	}

	return aModules
}

func (b *bottomUpMutatorContext) CreateLocalVariations(variations ...string) []Module {
	if b.finalPhase {
		panic("CreateLocalVariations not allowed in FinalDepsMutators")
	}

	modules := b.bp.CreateLocalVariations(variations...)

	aModules := make([]Module, len(modules))
	for i := range variations {
		aModules[i] = modules[i].(Module)
		base := aModules[i].base()
		base.commonProperties.DebugMutators = append(base.commonProperties.DebugMutators, b.MutatorName())
		base.commonProperties.DebugVariations = append(base.commonProperties.DebugVariations, variations[i])
	}

	return aModules
}

func (b *bottomUpMutatorContext) SetDependencyVariation(variation string) {
	b.bp.SetDependencyVariation(variation)
}

func (b *bottomUpMutatorContext) SetDefaultDependencyVariation(variation *string) {
	b.bp.SetDefaultDependencyVariation(variation)
}

func (b *bottomUpMutatorContext) AddVariationDependencies(variations []blueprint.Variation, tag blueprint.DependencyTag,
	names ...string) {

	b.bp.AddVariationDependencies(variations, tag, names...)
}

func (b *bottomUpMutatorContext) AddFarVariationDependencies(variations []blueprint.Variation,
	tag blueprint.DependencyTag, names ...string) {

	b.bp.AddFarVariationDependencies(variations, tag, names...)
}

func (b *bottomUpMutatorContext) AddInterVariantDependency(tag blueprint.DependencyTag, from, to blueprint.Module) {
	b.bp.AddInterVariantDependency(tag, from, to)
}

func (b *bottomUpMutatorContext) ReplaceDependencies(name string) {
	b.bp.ReplaceDependencies(name)
}

func (b *bottomUpMutatorContext) ReplaceDependenciesIf(name string, predicate blueprint.ReplaceDependencyPredicate) {
	b.bp.ReplaceDependenciesIf(name, predicate)
}

func (b *bottomUpMutatorContext) AliasVariation(variationName string) {
	b.bp.AliasVariation(variationName)
}

func (b *bottomUpMutatorContext) CreateAliasVariation(fromVariationName, toVariationName string) {
	b.bp.CreateAliasVariation(fromVariationName, toVariationName)
}
