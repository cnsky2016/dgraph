/*
 * Copyright 2020 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v200"
	"github.com/dgraph-io/dgo/v200/protos/api"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/x"
	"google.golang.org/grpc"
)

var (
	reservedNameError = fmt.Errorf("new name can't start with `dgraph.`, please try again! ")
	existingNameError = fmt.Errorf("new name can't be same as a name in existing schema, " +
		"please try again! ")
)

// getDgoClient creates a gRPC connection and uses that to create a new dgo client.
// The gRPC.ClientConn returned by this must be closed after use.
func getDgoClient(withLogin bool) (*dgo.Dgraph, *grpc.ClientConn, error) {
	alpha := Upgrade.Conf.GetString(alpha)

	// TODO(Aman): add TLS configuration.
	conn, err := grpc.Dial(alpha, grpc.WithInsecure())
	if err != nil {
		return nil, nil, fmt.Errorf("unable to connect to Dgraph cluster: %w", err)
	}

	dg := dgo.NewDgraphClient(api.NewDgraphClient(conn))

	if withLogin {
		userName := Upgrade.Conf.GetString(user)
		password := Upgrade.Conf.GetString(password)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// login to cluster
		if err = dg.Login(ctx, userName, password); err != nil {
			x.Check(conn.Close())
			return nil, nil, fmt.Errorf("unable to login to Dgraph cluster: %w", err)
		}
	}

	return dg, conn, nil
}

// getQueryResult executes the given query and unmarshals the result in given pointer queryResPtr.
// If any error is encountered, it returns the error.
func getQueryResult(dg *dgo.Dgraph, query string, queryResPtr interface{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := dg.NewReadOnlyTxn().Query(ctx, query)
	if err != nil {
		return err
	}

	return json.Unmarshal(resp.GetJson(), queryResPtr)
}

// mutateWithClient uses the given dgraph client to execute the given mutation.
// It retries max 3 times before returning failure error, if any.
func mutateWithClient(dg *dgo.Dgraph, mutation *api.Mutation) error {
	if mutation == nil {
		return nil
	}

	mutation.CommitNow = true

	var err error
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err = dg.NewTxn().Mutate(ctx, mutation)
		if err != nil {
			fmt.Println("error in running mutation, retrying:", err)
			continue
		}

		return nil
	}

	return err
}

// alterWithClient uses the given dgraph client to execute the given alter operation.
// It retries max 3 times before returning failure error, if any.
func alterWithClient(dg *dgo.Dgraph, operation *api.Operation) error {
	if operation == nil {
		return nil
	}

	var err error
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err = dg.Alter(ctx, operation)
		if err != nil {
			fmt.Println("error in alter, retrying:", err)
			continue
		}

		return nil
	}

	return err
}

// copyMap returns a copy of the input map
func copyMap(m map[string]interface{}) map[string]interface{} {
	m1 := make(map[string]interface{})
	for k, v := range m {
		m1[k] = v
	}
	return m1
}

// askUserForNewName prompts the user to input a new name on the terminal,
// and validates that the user-provided name is not reserved as well as doesn't exist in the
// existingNameMap argument. It will only return the newName if the user provides a valid name,
// otherwise it will ask the user to keep trying again until a valid name is not obtained.
func askUserForNewName(r io.Reader, w io.Writer, oldName string,
	checkReservedFunc func(string) bool, existingNameMap map[string]struct{}) string {
	var newName string

	// until the user doesn't supply a valid name, keep asking him
	for {
		x.Check2(fmt.Fprintf(w, "Enter new name for `%s`: ", oldName))
		if _, err := fmt.Fscan(r, &newName); err != nil {
			x.Check2(fmt.Fprintln(w, "Something went wrong while scanning input: ", err))
			x.Check2(fmt.Fprintln(w, "Try again!"))
			continue
		}
		if checkReservedFunc(newName) {
			x.Check2(fmt.Fprintln(w, reservedNameError))
			continue
		}
		if _, ok := existingNameMap[newName]; ok {
			x.Check2(fmt.Fprintln(w, existingNameError))
			continue
		}
		// if no error encountered, means name is valid, so break
		break
	}

	return newName
}

// getPredSchemaString generates a string which can be used to alter the schema for a predicate.
// It uses newPredName as the name of the predicate, other things are same as what is provided in
// schemaNode argument.
func getPredSchemaString(newPredName string, schemaNode *pb.SchemaNode) string {
	var builder strings.Builder
	builder.WriteString(newPredName)
	builder.WriteString(": ")

	if schemaNode.List {
		builder.WriteString("[")
	}
	builder.WriteString(schemaNode.Type)
	if schemaNode.List {
		builder.WriteString("]")
	}
	builder.WriteString(" ")

	if schemaNode.Count {
		builder.WriteString("@count ")
	}
	if schemaNode.Index {
		builder.WriteString("@index(")
		comma := ""
		for _, tokenizer := range schemaNode.Tokenizer {
			builder.WriteString(comma)
			builder.WriteString(tokenizer)
			comma = ", "
		}
		builder.WriteString(") ")
	}
	if schemaNode.Lang {
		builder.WriteString("@lang ")
	}
	if schemaNode.NoConflict {
		builder.WriteString("@noconflict ")
	}
	if schemaNode.Reverse {
		builder.WriteString("@reverse ")
	}
	if schemaNode.Upsert {
		builder.WriteString("@upsert ")
	}

	builder.WriteString(".\n")

	return builder.String()
}

// getTypeSchemaString generates a string which can be used to alter a type in schema.
// It generates the type string using new type and predicate names. So, if this type
// previously had a predicate for which we got a new name, then the generated string
// will contain the new name for that predicate. Also, if some predicates need to be
// removed from the type, then they can be supplied in predsToRemove. For example:
// initialType:
// 	type Person {
// 		name
// 		age
// 		unnecessaryEdge
// 	}
// also,
// 	newTypeName = "Human"
// 	newPredNames = {
// 		"age": "ageOnEarth'
// 	}
// 	predsToRemove = {
// 		"unnecessaryEdge": {}
// 	}
// then returned type string will be:
// 	type Human {
// 		name
// 		ageOnEarth
// 	}
func getTypeSchemaString(newTypeName string, typeNode *schemaTypeNode,
	newPredNames map[string]string, predsToRemove map[string]struct{}) string {
	var builder strings.Builder
	builder.WriteString("type ")
	builder.WriteString(newTypeName)
	builder.WriteString(" {\n")

	for _, oldPred := range typeNode.Fields {
		if _, ok := predsToRemove[oldPred.Name]; ok {
			continue
		}

		builder.WriteString("  ")
		newPredName, ok := newPredNames[oldPred.Name]
		if ok {
			builder.WriteString(newPredName)
		} else {
			builder.WriteString(oldPred.Name)
		}
		builder.WriteString("\n")
	}

	builder.WriteString("}\n")

	return builder.String()
}