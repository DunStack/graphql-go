package resolvable

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/graph-gophers/graphql-go/decode"
	"github.com/graph-gophers/graphql-go/directives"
	"github.com/graph-gophers/graphql-go/internal/exec/packer"
	"github.com/graph-gophers/graphql-go/types"
)

const (
	Query        = "Query"
	Mutation     = "Mutation"
	Subscription = "Subscription"
)

type Schema struct {
	*Meta
	types.Schema
	Query                Resolvable
	Mutation             Resolvable
	Subscription         Resolvable
	QueryResolver        reflect.Value
	MutationResolver     reflect.Value
	SubscriptionResolver reflect.Value
}

type Resolvable interface {
	isResolvable()
}

type Object struct {
	Name           string
	Fields         map[string]*Field
	TypeAssertions map[string]*TypeAssertion
}

type Field struct {
	types.FieldDefinition
	TypeName          string
	MethodIndex       int
	FieldIndex        []int
	HasContext        bool
	HasError          bool
	ArgsPacker        *packer.StructPacker
	DirectivesPackers map[string]*packer.StructPacker
	ValueExec         Resolvable
	TraceLabel        string
}

func (f *Field) UseMethodResolver() bool {
	return len(f.FieldIndex) == 0
}

type TypeAssertion struct {
	MethodIndex int
	TypeExec    Resolvable
}

type List struct {
	Elem Resolvable
}

type Scalar struct{}

func (*Object) isResolvable() {}
func (*List) isResolvable()   {}
func (*Scalar) isResolvable() {}

func ApplyResolver(s *types.Schema, resolver interface{}, dirVisitors []directives.Directive, useFieldResolvers bool) (*Schema, error) {
	if resolver == nil {
		return &Schema{Meta: newMeta(s), Schema: *s}, nil
	}

	ds, err := applyDirectives(s, dirVisitors)
	if err != nil {
		return nil, err
	}

	b := newBuilder(s, ds, useFieldResolvers)

	var query, mutation, subscription Resolvable

	resolvers := map[string]interface{}{}

	rv := reflect.ValueOf(resolver)
	// use separate resolvers in case Query, Mutation and/or Subscription methods are defined
	for _, op := range [...]string{Query, Mutation, Subscription} {
		m := rv.MethodByName(op)
		if m.IsValid() { // if the root resolver has a method for the current operation
			mt := m.Type()
			if mt.NumIn() != 0 {
				return nil, fmt.Errorf("method %q of %v must not accept any arguments, got %d", op, rv.Type(), mt.NumIn())
			}
			if mt.NumOut() != 1 {
				return nil, fmt.Errorf("method %q of %v must have 1 return value, got %d", op, rv.Type(), mt.NumOut())
			}
			ot := mt.Out(0)
			if ot.Kind() != reflect.Pointer && ot.Kind() != reflect.Interface {
				return nil, fmt.Errorf("method %q of %v must return an interface or a pointer, got %+v", op, rv.Type(), ot)
			}
			out := m.Call(nil)
			res := out[0]
			if res.IsNil() {
				return nil, fmt.Errorf("method %q of %v must return a non-nil result, got %v", op, rv.Type(), res)
			}
			switch res.Kind() {
			case reflect.Pointer:
				resolvers[op] = res.Elem().Addr().Interface()
			case reflect.Interface:
				resolvers[op] = res.Elem().Interface()
			default:
				panic("ureachable")
			}
		}
		// If a method for the current operation is not defined in the root resolver,
		// then use the root resolver for the operation.
		if resolvers[op] == nil {
			resolvers[op] = resolver
		}
	}

	if t, ok := s.RootOperationTypes["query"]; ok {
		if err := b.assignExec(&query, t, reflect.TypeOf(resolvers[Query])); err != nil {
			return nil, err
		}
	}

	if t, ok := s.RootOperationTypes["mutation"]; ok {
		if err := b.assignExec(&mutation, t, reflect.TypeOf(resolvers[Mutation])); err != nil {
			return nil, err
		}
	}

	if t, ok := s.RootOperationTypes["subscription"]; ok {
		if err := b.assignExec(&subscription, t, reflect.TypeOf(resolvers[Subscription])); err != nil {
			return nil, err
		}
	}

	if err := b.finish(); err != nil {
		return nil, err
	}

	return &Schema{
		Meta:                 newMeta(s),
		Schema:               *s,
		QueryResolver:        reflect.ValueOf(resolvers[Query]),
		MutationResolver:     reflect.ValueOf(resolvers[Mutation]),
		SubscriptionResolver: reflect.ValueOf(resolvers[Subscription]),
		Query:                query,
		Mutation:             mutation,
		Subscription:         subscription,
	}, nil
}

func applyDirectives(s *types.Schema, visitors []directives.Directive) (map[string]directives.Directive, error) {
	byName := make(map[string]directives.Directive, len(s.Directives))

	for _, v := range visitors {
		name := v.ImplementsDirective()

		if existing, ok := byName[name]; ok {
			return nil, fmt.Errorf("multiple implementations registered for directive %q. Implementation types %T and %T", name, existing, v)
		}

		// At least 1 of the optional directive functions must be defined for each directive.
		// For now this is the only valid directive function
		if _, ok := v.(directives.ResolverInterceptor); !ok {
			return nil, fmt.Errorf("directive %q (implemented by %T) does not implement a valid directive visitor function", name, v)
		}

		byName[name] = v
	}

	for name, def := range s.Directives {
		// TODO: directives other than FIELD_DEFINITION also need to be supported, and later addition of
		// capabilities to 'visit' other kinds of directive locations shouldn't break the parsing of existing
		// schemas that declare those directives, but don't have a visitor for them?
		var acceptedType bool
		for _, l := range def.Locations {
			if l == "FIELD_DEFINITION" {
				acceptedType = true
				break
			}
		}

		if !acceptedType {
			continue
		}

		if _, ok := byName[name]; !ok {
			if name == "include" || name == "skip" || name == "deprecated" || name == "specifiedBy" {
				// Special case directives, ignore
				continue
			}

			return nil, fmt.Errorf("no visitors have been registered for directive %q", name)
		}
	}

	return byName, nil
}

type execBuilder struct {
	schema            *types.Schema
	resMap            map[typePair]*resMapEntry
	directives        map[string]directives.Directive
	packerBuilder     *packer.Builder
	useFieldResolvers bool
}

type typePair struct {
	graphQLType  types.Type
	resolverType reflect.Type
}

type resMapEntry struct {
	exec    Resolvable
	targets []*Resolvable
}

func newBuilder(s *types.Schema, directives map[string]directives.Directive, useFieldResolvers bool) *execBuilder {
	return &execBuilder{
		schema:            s,
		resMap:            make(map[typePair]*resMapEntry),
		directives:        directives,
		packerBuilder:     packer.NewBuilder(),
		useFieldResolvers: useFieldResolvers,
	}
}

func (b *execBuilder) finish() error {
	for _, entry := range b.resMap {
		for _, target := range entry.targets {
			*target = entry.exec
		}
	}

	return b.packerBuilder.Finish()
}

func (b *execBuilder) assignExec(target *Resolvable, t types.Type, resolverType reflect.Type) error {
	k := typePair{t, resolverType}
	ref, ok := b.resMap[k]
	if !ok {
		ref = &resMapEntry{}
		b.resMap[k] = ref
		var err error
		ref.exec, err = b.makeExec(t, resolverType)
		if err != nil {
			return err
		}
	}
	ref.targets = append(ref.targets, target)
	return nil
}

func (b *execBuilder) makeExec(t types.Type, resolverType reflect.Type) (Resolvable, error) {
	var nonNull bool
	t, nonNull = unwrapNonNull(t)

	switch t := t.(type) {
	case *types.ObjectTypeDefinition:
		return b.makeObjectExec(t.Name, t.Fields, nil, nonNull, resolverType)

	case *types.InterfaceTypeDefinition:
		return b.makeObjectExec(t.Name, t.Fields, t.PossibleTypes, nonNull, resolverType)

	case *types.Union:
		return b.makeObjectExec(t.Name, nil, t.UnionMemberTypes, nonNull, resolverType)
	}

	if !nonNull {
		if resolverType.Kind() != reflect.Ptr {
			return nil, fmt.Errorf("%s is not a pointer", resolverType)
		}
		resolverType = resolverType.Elem()
	}

	switch t := t.(type) {
	case *types.ScalarTypeDefinition:
		return makeScalarExec(t, resolverType)

	case *types.EnumTypeDefinition:
		return &Scalar{}, nil

	case *types.List:
		if resolverType.Kind() != reflect.Slice {
			return nil, fmt.Errorf("%s is not a slice", resolverType)
		}
		e := &List{}
		if err := b.assignExec(&e.Elem, t.OfType, resolverType.Elem()); err != nil {
			return nil, err
		}
		return e, nil

	default:
		panic("invalid type: " + t.String())
	}
}

func makeScalarExec(t *types.ScalarTypeDefinition, resolverType reflect.Type) (Resolvable, error) {
	implementsType := false
	switch r := reflect.New(resolverType).Interface().(type) {
	case *int32:
		implementsType = t.Name == "Int"
	case *float64:
		implementsType = t.Name == "Float"
	case *string:
		implementsType = t.Name == "String"
	case *bool:
		implementsType = t.Name == "Boolean"
	case decode.Unmarshaler:
		implementsType = r.ImplementsGraphQLType(t.Name)
	}

	if !implementsType {
		return nil, fmt.Errorf("can not use %s as %s", resolverType, t.Name)
	}
	return &Scalar{}, nil
}

func (b *execBuilder) makeObjectExec(typeName string, fields types.FieldsDefinition, possibleTypes []*types.ObjectTypeDefinition,
	nonNull bool, resolverType reflect.Type) (*Object, error) {
	if !nonNull {
		if resolverType.Kind() != reflect.Ptr && resolverType.Kind() != reflect.Interface {
			return nil, fmt.Errorf("%s is not a pointer or interface", resolverType)
		}
	}

	methodHasReceiver := resolverType.Kind() != reflect.Interface

	Fields := make(map[string]*Field)
	rt := unwrapPtr(resolverType)
	fieldsCount := fieldCount(rt, map[string]int{})
	for _, f := range fields {
		var fieldIndex []int
		methodIndex := findMethod(resolverType, f.Name)
		if b.useFieldResolvers && methodIndex == -1 {
			if fieldsCount[strings.ToLower(stripUnderscore(f.Name))] > 1 {
				return nil, fmt.Errorf("%s does not resolve %q: ambiguous field %q", resolverType, typeName, f.Name)
			}
			fieldIndex = findField(rt, f.Name, []int{})
		}
		if methodIndex == -1 && len(fieldIndex) == 0 {
			hint := ""
			if findMethod(reflect.PtrTo(resolverType), f.Name) != -1 {
				hint = " (hint: the method exists on the pointer type)"
			}
			return nil, fmt.Errorf("%s does not resolve %q: missing method for field %q%s", resolverType, typeName, f.Name, hint)
		}

		var m reflect.Method
		var sf reflect.StructField
		if methodIndex != -1 {
			m = resolverType.Method(methodIndex)
		} else {
			sf = rt.FieldByIndex(fieldIndex)
		}
		fe, err := b.makeFieldExec(typeName, f, m, sf, methodIndex, fieldIndex, methodHasReceiver)
		if err != nil {
			var resolverName string
			if methodIndex != -1 {
				resolverName = m.Name
			} else {
				resolverName = sf.Name
			}
			return nil, fmt.Errorf("%s\n\tused by (%s).%s", err, resolverType, resolverName)
		}
		Fields[f.Name] = fe
	}

	// Check type assertions when
	//	1) using method resolvers
	//	2) Or resolver is not an interface type
	typeAssertions := make(map[string]*TypeAssertion)
	if !b.useFieldResolvers || resolverType.Kind() != reflect.Interface {
		for _, impl := range possibleTypes {
			methodIndex := findMethod(resolverType, "To"+impl.Name)
			if methodIndex == -1 {
				return nil, fmt.Errorf("%s does not resolve %q: missing method %q to convert to %q", resolverType, typeName, "To"+impl.Name, impl.Name)
			}
			m := resolverType.Method(methodIndex)
			expectedIn := 0
			if methodHasReceiver {
				expectedIn = 1
			}
			if m.Type.NumIn() != expectedIn {
				return nil, fmt.Errorf("%s does not resolve %q: method %q should't have any arguments", resolverType, typeName, "To"+impl.Name)
			}
			if m.Type.NumOut() != 2 {
				return nil, fmt.Errorf("%s does not resolve %q: method %q should return a value and a bool indicating success", resolverType, typeName, "To"+impl.Name)
			}
			a := &TypeAssertion{
				MethodIndex: methodIndex,
			}
			if err := b.assignExec(&a.TypeExec, impl, resolverType.Method(methodIndex).Type.Out(0)); err != nil {
				return nil, err
			}
			typeAssertions[impl.Name] = a
		}
	}

	return &Object{
		Name:           typeName,
		Fields:         Fields,
		TypeAssertions: typeAssertions,
	}, nil
}

var contextType = reflect.TypeOf((*context.Context)(nil)).Elem()
var errorType = reflect.TypeOf((*error)(nil)).Elem()

func (b *execBuilder) makeFieldExec(typeName string, f *types.FieldDefinition, m reflect.Method, sf reflect.StructField,
	methodIndex int, fieldIndex []int, methodHasReceiver bool) (*Field, error) {

	var argsPacker *packer.StructPacker
	var hasError bool
	var hasContext bool

	// Validate resolver method only when there is one
	if methodIndex != -1 {
		in := make([]reflect.Type, m.Type.NumIn())
		for i := range in {
			in[i] = m.Type.In(i)
		}
		if methodHasReceiver {
			in = in[1:] // first parameter is receiver
		}

		hasContext = len(in) > 0 && in[0] == contextType
		if hasContext {
			in = in[1:]
		}

		if len(f.Arguments) > 0 {
			if len(in) == 0 {
				return nil, fmt.Errorf("must have `args struct { ... }` argument for field arguments")
			}
			var err error
			argsPacker, err = b.packerBuilder.MakeStructPacker(f.Arguments, in[0])
			if err != nil {
				return nil, err
			}
			in = in[1:]
		}

		if len(in) > 0 {
			return nil, fmt.Errorf("too many arguments")
		}

		maxNumOfReturns := 2
		if m.Type.NumOut() < maxNumOfReturns-1 {
			return nil, fmt.Errorf("too few return values")
		}

		if m.Type.NumOut() > maxNumOfReturns {
			return nil, fmt.Errorf("too many return values")
		}

		hasError = m.Type.NumOut() == maxNumOfReturns
		if hasError {
			if m.Type.Out(maxNumOfReturns-1) != errorType {
				return nil, fmt.Errorf(`must have "error" as its last return value`)
			}
		}
	}

	directivesPackers := map[string]*packer.StructPacker{}
	for _, d := range f.Directives {
		n := d.Name.Name

		// skip special directives without packers
		if n == "deprecated" {
			continue
		}

		v, ok := b.directives[n]
		if !ok {
			return nil, fmt.Errorf("directive %q on field %q does not have a visitor registered with the schema", n, f.Name)
		}

		if _, ok = v.(directives.ResolverInterceptor); !ok {
			// Directive doesn't apply at field resolution time, skip it
			continue
		}

		r := reflect.TypeOf(v)

		// The directive definition is needed here in order to get the arguments definition list.
		// d.Arguments wouldn't work in this case because it does not contain args type information.
		dd, ok := b.schema.Directives[n]
		if !ok {
			return nil, fmt.Errorf("directive definition %q is not defined in the schema", n)
		}
		p, err := b.packerBuilder.MakeStructPacker(dd.Arguments, r)
		if err != nil {
			return nil, err
		}

		directivesPackers[n] = p
	}

	fe := &Field{
		FieldDefinition:   *f,
		TypeName:          typeName,
		MethodIndex:       methodIndex,
		FieldIndex:        fieldIndex,
		HasContext:        hasContext,
		ArgsPacker:        argsPacker,
		DirectivesPackers: directivesPackers,
		HasError:          hasError,
		TraceLabel:        fmt.Sprintf("GraphQL field: %s.%s", typeName, f.Name),
	}

	var out reflect.Type
	if methodIndex != -1 {
		out = m.Type.Out(0)
		sub, ok := b.schema.RootOperationTypes["subscription"]
		if ok && typeName == sub.TypeName() && out.Kind() == reflect.Chan {
			out = m.Type.Out(0).Elem()
		}
	} else {
		out = sf.Type
	}
	if err := b.assignExec(&fe.ValueExec, f.Type, out); err != nil {
		return nil, err
	}

	return fe, nil
}

func findMethod(t reflect.Type, name string) int {
	for i := 0; i < t.NumMethod(); i++ {
		if strings.EqualFold(stripUnderscore(name), stripUnderscore(t.Method(i).Name)) {
			return i
		}
	}
	return -1
}

func findField(t reflect.Type, name string, index []int) []int {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)

		if field.Type.Kind() == reflect.Struct && field.Anonymous {
			newIndex := findField(field.Type, name, []int{i})
			if len(newIndex) > 1 {
				return append(index, newIndex...)
			}
		}

		if strings.EqualFold(stripUnderscore(name), stripUnderscore(field.Name)) {
			return append(index, i)
		}
	}

	return index
}

// fieldCount helps resolve ambiguity when more than one embedded struct contains fields with the same name.
func fieldCount(t reflect.Type, count map[string]int) map[string]int {
	if t.Kind() != reflect.Struct {
		return nil
	}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		fieldName := strings.ToLower(stripUnderscore(field.Name))

		if field.Type.Kind() == reflect.Struct && field.Anonymous {
			count = fieldCount(field.Type, count)
		} else {
			if _, ok := count[fieldName]; !ok {
				count[fieldName] = 0
			}
			count[fieldName]++
		}
	}

	return count
}

func unwrapNonNull(t types.Type) (types.Type, bool) {
	if nn, ok := t.(*types.NonNull); ok {
		return nn.OfType, true
	}
	return t, false
}

func stripUnderscore(s string) string {
	return strings.Replace(s, "_", "", -1)
}

func unwrapPtr(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Ptr {
		return t.Elem()
	}
	return t
}
