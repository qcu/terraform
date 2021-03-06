package terraform

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/hcl"
	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/config/module"
	"github.com/hashicorp/terraform/helper/experiment"
)

// InputMode defines what sort of input will be asked for when Input
// is called on Context.
type InputMode byte

const (
	// InputModeVar asks for all variables
	InputModeVar InputMode = 1 << iota

	// InputModeVarUnset asks for variables which are not set yet.
	// InputModeVar must be set for this to have an effect.
	InputModeVarUnset

	// InputModeProvider asks for provider variables
	InputModeProvider

	// InputModeStd is the standard operating mode and asks for both variables
	// and providers.
	InputModeStd = InputModeVar | InputModeProvider
)

var (
	// contextFailOnShadowError will cause Context operations to return
	// errors when shadow operations fail. This is only used for testing.
	contextFailOnShadowError = false

	// contextTestDeepCopyOnPlan will perform a Diff DeepCopy on every
	// Plan operation, effectively testing the Diff DeepCopy whenever
	// a Plan occurs. This is enabled for tests.
	contextTestDeepCopyOnPlan = false
)

// ContextOpts are the user-configurable options to create a context with
// NewContext.
type ContextOpts struct {
	Destroy            bool
	Diff               *Diff
	Hooks              []Hook
	Module             *module.Tree
	Parallelism        int
	State              *State
	StateFutureAllowed bool
	Providers          map[string]ResourceProviderFactory
	Provisioners       map[string]ResourceProvisionerFactory
	Shadow             bool
	Targets            []string
	Variables          map[string]interface{}

	UIInput UIInput
}

// Context represents all the context that Terraform needs in order to
// perform operations on infrastructure. This structure is built using
// NewContext. See the documentation for that.
//
// Extra functions on Context can be found in context_*.go files.
type Context struct {
	// Maintainer note: Anytime this struct is changed, please verify
	// that newShadowContext still does the right thing. Tests should
	// fail regardless but putting this note here as well.

	components contextComponentFactory
	destroy    bool
	diff       *Diff
	diffLock   sync.RWMutex
	hooks      []Hook
	module     *module.Tree
	sh         *stopHook
	shadow     bool
	state      *State
	stateLock  sync.RWMutex
	targets    []string
	uiInput    UIInput
	variables  map[string]interface{}

	l                   sync.Mutex // Lock acquired during any task
	parallelSem         Semaphore
	providerInputConfig map[string]map[string]interface{}
	runCh               <-chan struct{}
	stopCh              chan struct{}
	shadowErr           error
}

// NewContext creates a new Context structure.
//
// Once a Context is creator, the pointer values within ContextOpts
// should not be mutated in any way, since the pointers are copied, not
// the values themselves.
func NewContext(opts *ContextOpts) (*Context, error) {
	// Copy all the hooks and add our stop hook. We don't append directly
	// to the Config so that we're not modifying that in-place.
	sh := new(stopHook)
	hooks := make([]Hook, len(opts.Hooks)+1)
	copy(hooks, opts.Hooks)
	hooks[len(opts.Hooks)] = sh

	state := opts.State
	if state == nil {
		state = new(State)
		state.init()
	}

	// If our state is from the future, then error. Callers can avoid
	// this error by explicitly setting `StateFutureAllowed`.
	if !opts.StateFutureAllowed && state.FromFutureTerraform() {
		return nil, fmt.Errorf(
			"Terraform doesn't allow running any operations against a state\n"+
				"that was written by a future Terraform version. The state is\n"+
				"reporting it is written by Terraform '%s'.\n\n"+
				"Please run at least that version of Terraform to continue.",
			state.TFVersion)
	}

	// Explicitly reset our state version to our current version so that
	// any operations we do will write out that our latest version
	// has run.
	state.TFVersion = Version

	// Determine parallelism, default to 10. We do this both to limit
	// CPU pressure but also to have an extra guard against rate throttling
	// from providers.
	par := opts.Parallelism
	if par == 0 {
		par = 10
	}

	// Set up the variables in the following sequence:
	//    0 - Take default values from the configuration
	//    1 - Take values from TF_VAR_x environment variables
	//    2 - Take values specified in -var flags, overriding values
	//        set by environment variables if necessary. This includes
	//        values taken from -var-file in addition.
	variables := make(map[string]interface{})

	if opts.Module != nil {
		var err error
		variables, err = Variables(opts.Module, opts.Variables)
		if err != nil {
			return nil, err
		}
	}

	return &Context{
		components: &basicComponentFactory{
			providers:    opts.Providers,
			provisioners: opts.Provisioners,
		},
		destroy:   opts.Destroy,
		diff:      opts.Diff,
		hooks:     hooks,
		module:    opts.Module,
		shadow:    opts.Shadow,
		state:     state,
		targets:   opts.Targets,
		uiInput:   opts.UIInput,
		variables: variables,

		parallelSem:         NewSemaphore(par),
		providerInputConfig: make(map[string]map[string]interface{}),
		sh:                  sh,
	}, nil
}

type ContextGraphOpts struct {
	Validate bool
	Verbose  bool
}

// Graph returns the graph for this config.
func (c *Context) Graph(g *ContextGraphOpts) (*Graph, error) {
	return c.graphBuilder(g).Build(RootModulePath)
}

// GraphBuilder returns the GraphBuilder that will be used to create
// the graphs for this context.
func (c *Context) graphBuilder(g *ContextGraphOpts) GraphBuilder {
	return &BuiltinGraphBuilder{
		Root:         c.module,
		Diff:         c.diff,
		Providers:    c.components.ResourceProviders(),
		Provisioners: c.components.ResourceProvisioners(),
		State:        c.state,
		Targets:      c.targets,
		Destroy:      c.destroy,
		Validate:     g.Validate,
		Verbose:      g.Verbose,
	}
}

// ShadowError returns any errors caught during a shadow operation.
//
// A shadow operation is an operation run in parallel to a real operation
// that performs the same tasks using new logic on copied state. The results
// are compared to ensure that the new logic works the same as the old logic.
// The shadow never affects the real operation or return values.
//
// The result of the shadow operation are only available through this function
// call after a real operation is complete.
//
// For API consumers of Context, you can safely ignore this function
// completely if you have no interest in helping report experimental feature
// errors to Terraform maintainers. Otherwise, please call this function
// after every operation and report this to the user.
//
// IMPORTANT: Shadow errors are _never_ critical: they _never_ affect
// the real state or result of a real operation. They are purely informational
// to assist in future Terraform versions being more stable. Please message
// this effectively to the end user.
//
// This must be called only when no other operation is running (refresh,
// plan, etc.). The result can be used in parallel to any other operation
// running.
func (c *Context) ShadowError() error {
	return c.shadowErr
}

// Input asks for input to fill variables and provider configurations.
// This modifies the configuration in-place, so asking for Input twice
// may result in different UI output showing different current values.
func (c *Context) Input(mode InputMode) error {
	v := c.acquireRun("input")
	defer c.releaseRun(v)

	if mode&InputModeVar != 0 {
		// Walk the variables first for the root module. We walk them in
		// alphabetical order for UX reasons.
		rootConf := c.module.Config()
		names := make([]string, len(rootConf.Variables))
		m := make(map[string]*config.Variable)
		for i, v := range rootConf.Variables {
			names[i] = v.Name
			m[v.Name] = v
		}
		sort.Strings(names)
		for _, n := range names {
			// If we only care about unset variables, then if the variable
			// is set, continue on.
			if mode&InputModeVarUnset != 0 {
				if _, ok := c.variables[n]; ok {
					continue
				}
			}

			var valueType config.VariableType

			v := m[n]
			switch valueType = v.Type(); valueType {
			case config.VariableTypeUnknown:
				continue
			case config.VariableTypeMap:
				// OK
			case config.VariableTypeList:
				// OK
			case config.VariableTypeString:
				// OK
			default:
				panic(fmt.Sprintf("Unknown variable type: %#v", v.Type()))
			}

			// If the variable is not already set, and the variable defines a
			// default, use that for the value.
			if _, ok := c.variables[n]; !ok {
				if v.Default != nil {
					c.variables[n] = v.Default.(string)
					continue
				}
			}

			// this should only happen during tests
			if c.uiInput == nil {
				log.Println("[WARN] Content.uiInput is nil")
				continue
			}

			// Ask the user for a value for this variable
			var value string
			retry := 0
			for {
				var err error
				value, err = c.uiInput.Input(&InputOpts{
					Id:          fmt.Sprintf("var.%s", n),
					Query:       fmt.Sprintf("var.%s", n),
					Description: v.Description,
				})
				if err != nil {
					return fmt.Errorf(
						"Error asking for %s: %s", n, err)
				}

				if value == "" && v.Required() {
					// Redo if it is required, but abort if we keep getting
					// blank entries
					if retry > 2 {
						return fmt.Errorf("missing required value for %q", n)
					}
					retry++
					continue
				}

				break
			}

			// no value provided, so don't set the variable at all
			if value == "" {
				continue
			}

			decoded, err := parseVariableAsHCL(n, value, valueType)
			if err != nil {
				return err
			}

			if decoded != nil {
				c.variables[n] = decoded
			}
		}
	}

	if mode&InputModeProvider != 0 {
		// Build the graph
		graph, err := c.Graph(&ContextGraphOpts{Validate: true})
		if err != nil {
			return err
		}

		// Do the walk
		if _, err := c.walk(graph, nil, walkInput); err != nil {
			return err
		}
	}

	return nil
}

// Apply applies the changes represented by this context and returns
// the resulting state.
//
// In addition to returning the resulting state, this context is updated
// with the latest state.
func (c *Context) Apply() (*State, error) {
	v := c.acquireRun("apply")
	defer c.releaseRun(v)

	// Copy our own state
	c.state = c.state.DeepCopy()

	// Enable the new graph by default
	X_legacyGraph := experiment.Enabled(experiment.X_legacyGraph)

	// Build the graph.
	var graph *Graph
	var err error
	if !X_legacyGraph {
		graph, err = (&ApplyGraphBuilder{
			Module:       c.module,
			Diff:         c.diff,
			State:        c.state,
			Providers:    c.components.ResourceProviders(),
			Provisioners: c.components.ResourceProvisioners(),
			Destroy:      c.destroy,
		}).Build(RootModulePath)
	} else {
		graph, err = c.Graph(&ContextGraphOpts{Validate: true})
	}
	if err != nil {
		return nil, err
	}

	// Determine the operation
	operation := walkApply
	if c.destroy {
		operation = walkDestroy
	}

	// Walk the graph
	walker, err := c.walk(graph, graph, operation)
	if len(walker.ValidationErrors) > 0 {
		err = multierror.Append(err, walker.ValidationErrors...)
	}

	// Clean out any unused things
	c.state.prune()

	return c.state, err
}

// Plan generates an execution plan for the given context.
//
// The execution plan encapsulates the context and can be stored
// in order to reinstantiate a context later for Apply.
//
// Plan also updates the diff of this context to be the diff generated
// by the plan, so Apply can be called after.
func (c *Context) Plan() (*Plan, error) {
	v := c.acquireRun("plan")
	defer c.releaseRun(v)

	p := &Plan{
		Module:  c.module,
		Vars:    c.variables,
		State:   c.state,
		Targets: c.targets,
	}

	var operation walkOperation
	if c.destroy {
		operation = walkPlanDestroy
	} else {
		// Set our state to be something temporary. We do this so that
		// the plan can update a fake state so that variables work, then
		// we replace it back with our old state.
		old := c.state
		if old == nil {
			c.state = &State{}
			c.state.init()
		} else {
			c.state = old.DeepCopy()
		}
		defer func() {
			c.state = old
		}()

		operation = walkPlan
	}

	// Setup our diff
	c.diffLock.Lock()
	c.diff = new(Diff)
	c.diff.init()
	c.diffLock.Unlock()

	// Used throughout below
	X_legacyGraph := experiment.Enabled(experiment.X_legacyGraph)

	// Build the graph.
	var graph *Graph
	var err error
	if !X_legacyGraph {
		if c.destroy {
			graph, err = (&DestroyPlanGraphBuilder{
				Module:  c.module,
				State:   c.state,
				Targets: c.targets,
			}).Build(RootModulePath)
		} else {
			graph, err = (&PlanGraphBuilder{
				Module:    c.module,
				State:     c.state,
				Providers: c.components.ResourceProviders(),
				Targets:   c.targets,
			}).Build(RootModulePath)
		}
	} else {
		graph, err = c.Graph(&ContextGraphOpts{Validate: true})
	}
	if err != nil {
		return nil, err
	}

	// Do the walk
	walker, err := c.walk(graph, graph, operation)
	if err != nil {
		return nil, err
	}
	p.Diff = c.diff

	// If this is true, it means we're running unit tests. In this case,
	// we perform a deep copy just to ensure that all context tests also
	// test that a diff is copy-able. This will panic if it fails. This
	// is enabled during unit tests.
	//
	// This should never be true during production usage, but even if it is,
	// it can't do any real harm.
	if contextTestDeepCopyOnPlan {
		p.Diff.DeepCopy()
	}

	// We don't do the reverification during the new destroy plan because
	// it will use a different apply process.
	if X_legacyGraph {
		// Now that we have a diff, we can build the exact graph that Apply will use
		// and catch any possible cycles during the Plan phase.
		if _, err := c.Graph(&ContextGraphOpts{Validate: true}); err != nil {
			return nil, err
		}
	}

	var errs error
	if len(walker.ValidationErrors) > 0 {
		errs = multierror.Append(errs, walker.ValidationErrors...)
	}
	return p, errs
}

// Refresh goes through all the resources in the state and refreshes them
// to their latest state. This will update the state that this context
// works with, along with returning it.
//
// Even in the case an error is returned, the state will be returned and
// will potentially be partially updated.
func (c *Context) Refresh() (*State, error) {
	v := c.acquireRun("refresh")
	defer c.releaseRun(v)

	// Copy our own state
	c.state = c.state.DeepCopy()

	// Build the graph
	graph, err := c.Graph(&ContextGraphOpts{Validate: true})
	if err != nil {
		return nil, err
	}

	// Do the walk
	if _, err := c.walk(graph, graph, walkRefresh); err != nil {
		return nil, err
	}

	// Clean out any unused things
	c.state.prune()

	return c.state, nil
}

// Stop stops the running task.
//
// Stop will block until the task completes.
func (c *Context) Stop() {
	c.l.Lock()
	ch := c.runCh

	// If we aren't running, then just return
	if ch == nil {
		c.l.Unlock()
		return
	}

	// Tell the hook we want to stop
	c.sh.Stop()

	// Close the stop channel
	close(c.stopCh)

	// Wait for us to stop
	c.l.Unlock()
	<-ch
}

// Validate validates the configuration and returns any warnings or errors.
func (c *Context) Validate() ([]string, []error) {
	v := c.acquireRun("validate")
	defer c.releaseRun(v)

	var errs error

	// Validate the configuration itself
	if err := c.module.Validate(); err != nil {
		errs = multierror.Append(errs, err)
	}

	// This only needs to be done for the root module, since inter-module
	// variables are validated in the module tree.
	if config := c.module.Config(); config != nil {
		// Validate the user variables
		if err := smcUserVariables(config, c.variables); len(err) > 0 {
			errs = multierror.Append(errs, err...)
		}
	}

	// If we have errors at this point, the graphing has no chance,
	// so just bail early.
	if errs != nil {
		return nil, []error{errs}
	}

	// Build the graph so we can walk it and run Validate on nodes.
	// We also validate the graph generated here, but this graph doesn't
	// necessarily match the graph that Plan will generate, so we'll validate the
	// graph again later after Planning.
	graph, err := c.Graph(&ContextGraphOpts{Validate: true})
	if err != nil {
		return nil, []error{err}
	}

	// Walk
	walker, err := c.walk(graph, graph, walkValidate)
	if err != nil {
		return nil, multierror.Append(errs, err).Errors
	}

	// Return the result
	rerrs := multierror.Append(errs, walker.ValidationErrors...)
	return walker.ValidationWarnings, rerrs.Errors
}

// Module returns the module tree associated with this context.
func (c *Context) Module() *module.Tree {
	return c.module
}

// Variables will return the mapping of variables that were defined
// for this Context. If Input was called, this mapping may be different
// than what was given.
func (c *Context) Variables() map[string]interface{} {
	return c.variables
}

// SetVariable sets a variable after a context has already been built.
func (c *Context) SetVariable(k string, v interface{}) {
	c.variables[k] = v
}

func (c *Context) acquireRun(phase string) chan<- struct{} {
	c.l.Lock()
	defer c.l.Unlock()

	dbug.SetPhase(phase)

	// Wait for no channel to exist
	for c.runCh != nil {
		c.l.Unlock()
		ch := c.runCh
		<-ch
		c.l.Lock()
	}

	// Create the new channel
	ch := make(chan struct{})
	c.runCh = ch

	// Reset the stop channel so we can watch that
	c.stopCh = make(chan struct{})

	// Reset the stop hook so we're not stopped
	c.sh.Reset()

	// Reset the shadow errors
	c.shadowErr = nil

	return ch
}

func (c *Context) releaseRun(ch chan<- struct{}) {
	c.l.Lock()
	defer c.l.Unlock()

	// setting the phase to "INVALID" lets us easily detect if we have
	// operations happening outside of a run, or we missed setting the proper
	// phase
	dbug.SetPhase("INVALID")

	close(ch)
	c.runCh = nil
	c.stopCh = nil
}

func (c *Context) walk(
	graph, shadow *Graph, operation walkOperation) (*ContextGraphWalker, error) {
	// Keep track of the "real" context which is the context that does
	// the real work: talking to real providers, modifying real state, etc.
	realCtx := c

	// If we don't want shadowing, remove it
	if !experiment.Enabled(experiment.X_shadow) {
		shadow = nil
	}

	// If we have a shadow graph, walk that as well
	var shadowCtx *Context
	var shadowCloser Shadow
	if c.shadow && shadow != nil {
		// Build the shadow context. In the process, override the real context
		// with the one that is wrapped so that the shadow context can verify
		// the results of the real.
		realCtx, shadowCtx, shadowCloser = newShadowContext(c)
	}

	// Just log this so we can see it in a debug log
	if !c.shadow {
		log.Printf("[WARN] terraform: shadow graph disabled")
	}

	log.Printf("[DEBUG] Starting graph walk: %s", operation.String())

	walker := &ContextGraphWalker{
		Context:   realCtx,
		Operation: operation,
	}

	// Watch for a stop so we can call the provider Stop() API.
	doneCh := make(chan struct{})
	go c.watchStop(walker, c.stopCh, doneCh)

	// Walk the real graph, this will block until it completes
	realErr := graph.Walk(walker)

	// Close the done channel so the watcher stops
	close(doneCh)

	// If we have a shadow graph and we interrupted the real graph, then
	// we just close the shadow and never verify it. It is non-trivial to
	// recreate the exact execution state up until an interruption so this
	// isn't supported with shadows at the moment.
	if shadowCloser != nil && c.sh.Stopped() {
		// Ignore the error result, there is nothing we could care about
		shadowCloser.CloseShadow()

		// Set it to nil so we don't do anything
		shadowCloser = nil
	}

	// If we have a shadow graph, wait for that to complete.
	if shadowCloser != nil {
		// Build the graph walker for the shadow. We also wrap this in
		// a panicwrap so that panics are captured. For the shadow graph,
		// we just want panics to be normal errors rather than to crash
		// Terraform.
		shadowWalker := GraphWalkerPanicwrap(&ContextGraphWalker{
			Context:   shadowCtx,
			Operation: operation,
		})

		// Kick off the shadow walk. This will block on any operations
		// on the real walk so it is fine to start first.
		log.Printf("[INFO] Starting shadow graph walk: %s", operation.String())
		shadowCh := make(chan error)
		go func() {
			shadowCh <- shadow.Walk(shadowWalker)
		}()

		// Notify the shadow that we're done
		if err := shadowCloser.CloseShadow(); err != nil {
			c.shadowErr = multierror.Append(c.shadowErr, err)
		}

		// Wait for the walk to end
		log.Printf("[DEBUG] Waiting for shadow graph to complete...")
		shadowWalkErr := <-shadowCh

		// Get any shadow errors
		if err := shadowCloser.ShadowError(); err != nil {
			c.shadowErr = multierror.Append(c.shadowErr, err)
		}

		// Verify the contexts (compare)
		if err := shadowContextVerify(realCtx, shadowCtx); err != nil {
			c.shadowErr = multierror.Append(c.shadowErr, err)
		}

		// At this point, if we're supposed to fail on error, then
		// we PANIC. Some tests just verify that there is an error,
		// so simply appending it to realErr and returning could hide
		// shadow problems.
		//
		// This must be done BEFORE appending shadowWalkErr since the
		// shadowWalkErr may include expected errors.
		//
		// We only do this if we don't have a real error. In the case of
		// a real error, we can't guarantee what nodes were and weren't
		// traversed in parallel scenarios so we can't guarantee no
		// shadow errors.
		if c.shadowErr != nil && contextFailOnShadowError && realErr == nil {
			panic(multierror.Prefix(c.shadowErr, "shadow graph:"))
		}

		// Now, if we have a walk error, we append that through
		if shadowWalkErr != nil {
			c.shadowErr = multierror.Append(c.shadowErr, shadowWalkErr)
		}

		if c.shadowErr == nil {
			log.Printf("[INFO] Shadow graph success!")
		} else {
			log.Printf("[ERROR] Shadow graph error: %s", c.shadowErr)

			// If we're supposed to fail on shadow errors, then report it
			if contextFailOnShadowError {
				realErr = multierror.Append(realErr, multierror.Prefix(
					c.shadowErr, "shadow graph:"))
			}
		}
	}

	return walker, realErr
}

func (c *Context) watchStop(walker *ContextGraphWalker, stopCh, doneCh <-chan struct{}) {
	// Wait for a stop or completion
	select {
	case <-stopCh:
		// Stop was triggered. Fall out of the select
	case <-doneCh:
		// Done, just exit completely
		return
	}

	// If we're here, we're stopped, trigger the call.

	// Copy the providers so that a misbehaved blocking Stop doesn't
	// completely hang Terraform.
	walker.providerLock.Lock()
	ps := make([]ResourceProvider, 0, len(walker.providerCache))
	for _, p := range walker.providerCache {
		ps = append(ps, p)
	}
	defer walker.providerLock.Unlock()

	for _, p := range ps {
		// We ignore the error for now since there isn't any reasonable
		// action to take if there is an error here, since the stop is still
		// advisory: Terraform will exit once the graph node completes.
		p.Stop()
	}
}

// parseVariableAsHCL parses the value of a single variable as would have been specified
// on the command line via -var or in an environment variable named TF_VAR_x, where x is
// the name of the variable. In order to get around the restriction of HCL requiring a
// top level object, we prepend a sentinel key, decode the user-specified value as its
// value and pull the value back out of the resulting map.
func parseVariableAsHCL(name string, input string, targetType config.VariableType) (interface{}, error) {
	// expecting a string so don't decode anything, just strip quotes
	if targetType == config.VariableTypeString {
		return strings.Trim(input, `"`), nil
	}

	// return empty types
	if strings.TrimSpace(input) == "" {
		switch targetType {
		case config.VariableTypeList:
			return []interface{}{}, nil
		case config.VariableTypeMap:
			return make(map[string]interface{}), nil
		}
	}

	const sentinelValue = "SENTINEL_TERRAFORM_VAR_OVERRIDE_KEY"
	inputWithSentinal := fmt.Sprintf("%s = %s", sentinelValue, input)

	var decoded map[string]interface{}
	err := hcl.Decode(&decoded, inputWithSentinal)
	if err != nil {
		return nil, fmt.Errorf("Cannot parse value for variable %s (%q) as valid HCL: %s", name, input, err)
	}

	if len(decoded) != 1 {
		return nil, fmt.Errorf("Cannot parse value for variable %s (%q) as valid HCL. Only one value may be specified.", name, input)
	}

	parsedValue, ok := decoded[sentinelValue]
	if !ok {
		return nil, fmt.Errorf("Cannot parse value for variable %s (%q) as valid HCL. One value must be specified.", name, input)
	}

	switch targetType {
	case config.VariableTypeList:
		return parsedValue, nil
	case config.VariableTypeMap:
		if list, ok := parsedValue.([]map[string]interface{}); ok {
			return list[0], nil
		}

		return nil, fmt.Errorf("Cannot parse value for variable %s (%q) as valid HCL. One value must be specified.", name, input)
	default:
		panic(fmt.Errorf("unknown type %s", targetType.Printable()))
	}
}
