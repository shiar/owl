package owl

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// Saves all the built resolver trees without applying options.
// The key is the struct type.
var builtTrees sync.Map

// Resolver is a field resolver. Which is a node in the resolver tree.
// The resolver tree is built from a struct value. Each node represents a
// field in the struct. The root node represents the struct itself.
// It is used to resolve a field value from a data source.
type Resolver struct {
	Type       reflect.Type
	Field      reflect.StructField
	Index      int // field index in the parent struct
	Path       []string
	Directives []*Directive
	Parent     *Resolver
	Children   []*Resolver
	Context    context.Context // save custom resolver settings here
}

// New builds a resolver tree from a struct value. The given options will be
// applied to all the resolvers. In the resolver tree, each node is also a
// Resolver.
func New(structValue interface{}, opts ...Option) (*Resolver, error) {
	typ, err := reflectStructType(structValue)
	if err != nil {
		return nil, err
	}

	tree, err := buildAndCacheResolverTree(typ)
	if err != nil {
		return nil, err
	}
	tree = tree.copy()

	// Apply options to each resolver.
	opts = normalizeOptions(opts)
	if err := tree.Iterate(func(r *Resolver) error {
		for _, opt := range opts {
			if err := opt.Apply(r); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if err := tree.validate(); err != nil {
		return nil, err
	}

	return tree, nil
}

func (r *Resolver) copy() *Resolver {
	resolverCopy := new(Resolver)
	*resolverCopy = *r
	resolverCopy.Context = context.Background()

	// Copy the children.
	resolverCopy.Children = make([]*Resolver, len(r.Children))
	for i, child := range r.Children {
		resolverCopy.Children[i] = child.copy()
		resolverCopy.Children[i].Parent = resolverCopy
	}
	return resolverCopy
}

func (r *Resolver) validate() error {
	if r.Namespace() == nil {
		return fmt.Errorf("%w: the namespace you passed through WithNamespace to owl.New is nil", ErrNilNamespace)
	}

	return nil
}

func (r *Resolver) IsRoot() bool {
	return r.Parent == nil
}

func (r *Resolver) IsLeaf() bool {
	return len(r.Children) == 0
}

func (r *Resolver) PathString() string {
	return strings.Join(r.Path, ".")
}

func (r *Resolver) GetDirective(name string) *Directive {
	for _, d := range r.Directives {
		if d.Name == name {
			return d
		}
	}
	return nil
}

func (r *Resolver) Namespace() *Namespace {
	return r.Context.Value(ckNamespace).(*Namespace)
}

// Find finds a field resolver by path. e.g. "Pagination.Page", "User.Name", etc.
func (r *Resolver) Lookup(path string) *Resolver {
	return findResolver(r, strings.Split(path, "."))
}

func findResolver(root *Resolver, path []string) *Resolver {
	if len(path) == 0 {
		return root
	}

	for _, field := range root.Children {
		if field.Field.Name == path[0] {
			return findResolver(field, path[1:])
		}
	}

	return nil
}

func (r *Resolver) String() string {
	return fmt.Sprintf("%s (%v)", r.PathString(), r.Type)
}

// Iterate iterates the resolver tree by depth-first. The callback function
// will be called for each field resolver. If the callback returns an error,
// the iteration will be stopped.
func (r *Resolver) Iterate(fn func(*Resolver) error) error {
	return iterateTree(r, fn)
}

func iterateTree(root *Resolver, fn func(*Resolver) error) error {
	if err := fn(root); err != nil {
		return err
	}

	for _, field := range root.Children {
		if err := iterateTree(field, fn); err != nil {
			return err
		}
	}

	return nil
}

// Resolve resolves the resolver tree from a data source.
// It iterates the tree by depth-first, and runs the directives on each field.
func (r *Resolver) Resolve(opts ...ResolveOption) (reflect.Value, error) {
	ctx := context.Background()
	// Apply resolve options.
	for _, opt := range opts {
		ctx = opt.Apply(ctx)
	}

	return r.resolve(ctx)
}

func (root *Resolver) resolve(ctx context.Context) (reflect.Value, error) {
	rootValue := reflect.New(root.Type)

	// Run the directives on current field.
	if err := root.runDirectives(ctx, rootValue); err != nil {
		return rootValue, err
	}

	// Resolve the children fields.
	if len(root.Children) > 0 {
		// If the root is a pointer, we need to allocate memory for it.
		// We only expect it's a one-level pointer, e.g. *User, not **User.
		underlyingValue := rootValue
		if root.Type.Kind() == reflect.Ptr {
			underlyingValue = reflect.New(root.Type.Elem())
			rootValue.Elem().Set(underlyingValue)
		}

		for _, child := range root.Children {
			fieldValue, err := child.resolve(ctx)
			if err != nil {
				return rootValue, &ResolveError{
					Err:      err,
					Resolver: child,
				}
			}
			underlyingValue.Elem().Field(child.Index).Set(fieldValue.Elem())
		}
	}

	return rootValue, nil
}

func (r *Resolver) runDirectives(ctx context.Context, rv reflect.Value) error {
	ns := r.Namespace()

	for _, directive := range r.Directives {
		dirRuntime := &DirectiveRuntime{
			Directive: directive,
			Resolver:  r,
			Context:   ctx,
			Value:     rv,
		}
		exe := ns.LookupExecutor(directive.Name)
		if exe == nil {
			return &DirectiveExecutionError{
				Err:       ErrMissingExecutor,
				Directive: *directive,
			}
		}

		if err := exe.Execute(dirRuntime); err != nil {
			return &DirectiveExecutionError{
				Err:       err,
				Directive: *directive,
			}
		}
	}

	return nil
}

func (r *Resolver) DebugLayoutText(depth int) string {
	var sb strings.Builder
	sb.WriteString(r.String())
	sb.WriteString(fmt.Sprintf("  %v", r.Index))

	for i, field := range r.Children {
		sb.WriteString("\n")
		sb.WriteString(strings.Repeat("    ", depth+1))
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString("# ")
		sb.WriteString(field.DebugLayoutText(depth + 1))
	}
	return sb.String()
}

// buildAndCacheResolverTree returns the tree with minimum settings (without any
// options applied). It will load from cache if possible. Otherwise, it will
// build the tree from scratch and cache it.
func buildAndCacheResolverTree(typ reflect.Type) (tree *Resolver, err error) {
	if builtTree, ok := builtTrees.Load(typ); ok {
		return builtTree.(*Resolver), nil
	}

	tree, err = buildResolverTree(typ)
	if err != nil {
		return nil, err
	}

	// Build successfully, cache it (must a copy).
	builtTrees.Store(typ, tree)
	return tree, nil
}

// buildResolverTree builds a resolver tree from a struct type.
func buildResolverTree(st reflect.Type) (*Resolver, error) {
	return buildResolver(st, reflect.StructField{}, nil)
}

func buildResolver(t reflect.Type, field reflect.StructField, parent *Resolver) (*Resolver, error) {
	root := &Resolver{
		Type:    t,
		Field:   field,
		Index:   -1,
		Parent:  parent,
		Context: context.Background(),
	}

	if !root.IsRoot() {
		directives, err := parseDirectives(field.Tag.Get(Tag()))
		if err != nil {
			return nil, fmt.Errorf("parse directives: %w", err)
		}
		root.Directives = directives
		root.Path = append(root.Parent.Path, field.Name)
	}

	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	if t.Kind() == reflect.Struct {
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)

			// Skip unexported fields. Because we can't set value to them, nor
			// get value from them by reflection.
			if !field.IsExported() {
				continue
			}

			child, err := buildResolver(field.Type, field, root)
			if err != nil {
				path := append(root.Path, field.Name)
				return nil, fmt.Errorf("build resolver for %q failed: %w", strings.Join(path, "."), err)
			}
			child.Index = i
			root.Children = append(root.Children, child)
		}
	}
	return root, nil
}

func reflectStructType(structValue interface{}) (reflect.Type, error) {
	typ, ok := structValue.(reflect.Type)
	if !ok {
		typ = reflect.TypeOf(structValue)
	}

	if typ == nil {
		return nil, fmt.Errorf("nil type")
	}

	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}

	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("non-struct type: %v", typ)
	}

	return typ, nil
}

func parseDirectives(tag string) ([]*Directive, error) {
	tag = strings.TrimSpace(tag)
	var directives []*Directive
	existed := make(map[string]bool)
	for _, directive := range strings.Split(tag, ";") {
		directive = strings.TrimSpace(directive)
		if directive == "" {
			continue
		}
		d, err := ParseDirective(directive)
		if err != nil {
			return nil, err
		}
		if existed[d.Name] {
			return nil, duplicateDirective(d.Name)
		}
		existed[d.Name] = true
		directives = append(directives, d)
	}
	return directives, nil
}
